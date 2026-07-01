package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Route is the destination of an outbound message. The dispatcher owns it and
// pins it per invocation; a responder never chooses it.
type Route struct {
	ChatID  int64
	ReplyTo int64 // message_id to reply to; 0 = none
}

// Sender delivers rendered messages to Telegram. Client implements it; tests
// use a fake. The Send* calls return the sent message_id (for the future
// reply-resurrection track); SendChatAction is best-effort UX with no id.
type Sender interface {
	SendMessage(ctx context.Context, r Route, text, parseMode string, silent bool) (messageID int64, err error)
	SendDocument(ctx context.Context, r Route, path, filename, caption, parseMode string, silent bool) (messageID int64, err error)
	SendChatAction(ctx context.Context, chatID int64, action string) error
}

// Update is a single Telegram update (only message updates are requested).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is a Telegram message (the fields the dispatcher needs).
type Message struct {
	MessageID int64    `json:"message_id"`
	Text      string   `json:"text"`
	Chat      Chat     `json:"chat"`
	From      *User    `json:"from"`
	ReplyTo   *Message `json:"reply_to_message"`
}

// Chat identifies the conversation an update belongs to.
type Chat struct {
	ID int64 `json:"id"`
}

// User is the sender of a message (only id/username are used, for logging).
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Client talks to the Telegram Bot API. It holds the bot token — the dispatcher
// is the only component that does.
type Client struct {
	Token   string
	BaseURL string // default https://api.telegram.org (overridden in tests)
	HTTP    *http.Client
}

// NewClient returns a Client for token with production defaults.
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: "https://api.telegram.org",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.telegram.org"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) methodURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL(), c.Token, method)
}

// GetUpdates long-polls for new updates starting at offset (last seen
// update_id + 1), waiting up to timeoutSec for one to arrive.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message"},
	}
	status, body, err := c.postJSON(ctx, "getUpdates", payload)
	if err != nil {
		return nil, err
	}
	var r struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		Result      []Update `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decoding getUpdates (HTTP %d): %w", status, err)
	}
	if !r.OK {
		return nil, fmt.Errorf("getUpdates error (HTTP %d): %s", status, r.Description)
	}
	return r.Result, nil
}

// SendMessage delivers a text message (sendMessage).
func (c *Client) SendMessage(ctx context.Context, r Route, text, parseMode string, silent bool) (int64, error) {
	payload := map[string]any{
		"chat_id": r.ChatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if r.ReplyTo != 0 {
		payload["reply_to_message_id"] = r.ReplyTo
	}
	if silent {
		payload["disable_notification"] = true
	}
	status, body, err := c.postJSON(ctx, "sendMessage", payload)
	if err != nil {
		return 0, err
	}
	return decodeMessageID(status, body)
}

// BotCommand is one entry of the bot's command menu (setMyCommands): the command
// name without a leading slash, plus the description clients show in the "/" list.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands uploads the bot's command menu (the "/" list clients show),
// replacing any previously set commands for the default scope. Best-effort at
// startup — the bot works without a menu, so the caller may just log a failure.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	payload := map[string]any{"commands": commands}
	status, body, err := c.postJSON(ctx, "setMyCommands", payload)
	if err != nil {
		return err
	}
	return checkOK(status, body)
}

// SendChatAction shows a chat action (e.g. "typing") in chat. Telegram clears
// it after ~5s, or immediately when the bot next sends a message, so a caller
// that wants it visible must refresh it. It is best-effort UX — the caller may
// ignore the error.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	status, body, err := c.postJSON(ctx, "sendChatAction", payload)
	if err != nil {
		return err
	}
	return checkOK(status, body)
}

// SendDocument uploads a file as an attachment (sendDocument, multipart).
func (c *Client) SendDocument(ctx context.Context, r Route, path, filename, caption, parseMode string, silent bool) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening attachment: %w", err)
	}
	defer f.Close()
	if filename == "" {
		filename = filepath.Base(path)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", strconv.FormatInt(r.ChatID, 10))
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	if parseMode != "" {
		_ = mw.WriteField("parse_mode", parseMode)
	}
	if r.ReplyTo != 0 {
		_ = mw.WriteField("reply_to_message_id", strconv.FormatInt(r.ReplyTo, 10))
	}
	if silent {
		_ = mw.WriteField("disable_notification", "true")
	}
	part, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL("sendDocument"), &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	status, body, err := c.do(req)
	if err != nil {
		return 0, err
	}
	return decodeMessageID(status, body)
}

// postJSON POSTs a JSON payload to a Bot API method and returns the raw body.
func (c *Client) postJSON(ctx context.Context, method string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

// do executes a prepared request and returns its status and body. Transport
// errors are returned; HTTP/API status is left to the caller (the Telegram
// envelope carries ok/description).
func (c *Client) do(req *http.Request) (int, []byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// apiErrorParams carries the optional parameters object of a non-OK Bot API
// envelope; only retry_after (429) is consumed.
type apiErrorParams struct {
	RetryAfter int `json:"retry_after"`
}

// APIError is a structured non-OK Telegram Bot API response. The dispatcher's
// drain type-asserts it (errors.As) to classify a send failure as permanent (a
// 4xx the responder must fix) or transient (429 / 5xx — retry with back-off);
// RetryAfter carries an authoritative 429 back-off. Error() keeps the
// "(HTTP <n>): <description>" shape older tests match on.
type APIError struct {
	Code        int    // error_code from the body, else the HTTP status
	Description string // Telegram's human-readable description
	RetryAfter  int    // parameters.retry_after in seconds (429 only)
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Telegram API error (HTTP %d): %s", e.Code, e.Description)
}

// pickCode prefers the body's own error_code and falls back to the HTTP status
// when the envelope omits it.
func pickCode(errorCode, status int) int {
	if errorCode != 0 {
		return errorCode
	}
	return status
}

// newAPIError assembles an *APIError from a decoded non-OK envelope.
func newAPIError(status, errorCode int, description string, params *apiErrorParams) *APIError {
	e := &APIError{Code: pickCode(errorCode, status), Description: description}
	if params != nil {
		e.RetryAfter = params.RetryAfter
	}
	return e
}

// checkOK verifies a Bot API response envelope carries ok:true (for methods
// whose result payload the caller does not need, e.g. sendChatAction).
func checkOK(status int, body []byte) error {
	var ar struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  *apiErrorParams `json:"parameters"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return fmt.Errorf("decoding Telegram response (HTTP %d): %w", status, err)
	}
	if !ar.OK {
		return newAPIError(status, ar.ErrorCode, ar.Description, ar.Parameters)
	}
	return nil
}

// decodeMessageID unwraps the {ok, result:{message_id}} envelope.
func decodeMessageID(status int, body []byte) (int64, error) {
	var ar struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  *apiErrorParams `json:"parameters"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return 0, fmt.Errorf("decoding Telegram response (HTTP %d): %w", status, err)
	}
	if !ar.OK {
		return 0, newAPIError(status, ar.ErrorCode, ar.Description, ar.Parameters)
	}
	return ar.Result.MessageID, nil
}
