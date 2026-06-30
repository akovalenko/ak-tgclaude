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
// use a fake. Both calls return the sent message_id (for the future
// reply-resurrection track).
type Sender interface {
	SendMessage(ctx context.Context, r Route, text, parseMode string, silent bool) (messageID int64, err error)
	SendDocument(ctx context.Context, r Route, path, filename, caption, parseMode string, silent bool) (messageID int64, err error)
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

// apiResponse is the envelope Telegram wraps every result in.
type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
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
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
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
	return c.do(req)
}

// do executes a prepared request and unwraps the Telegram envelope.
func (c *Client) do(req *http.Request) (int64, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return 0, fmt.Errorf("decoding Telegram response (HTTP %d): %w", resp.StatusCode, err)
	}
	if !ar.OK {
		return 0, fmt.Errorf("Telegram API error (HTTP %d): %s", resp.StatusCode, ar.Description)
	}
	return ar.Result.MessageID, nil
}
