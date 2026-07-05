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
	"strings"
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
	MessageID int64       `json:"message_id"`
	Date      int64       `json:"date"`     // Unix send time (seconds); stamped into the prompt for temporal orientation
	Text      string      `json:"text"`     // text messages
	Caption   string      `json:"caption"`  // media messages carry the user's text here, not in Text
	Document  *Document   `json:"document"` // an attached file (nil for a plain text message)
	Photo     []PhotoSize `json:"photo"`    // an attached photo, as several renditions (empty if none)
	Chat      Chat        `json:"chat"`
	From      *User       `json:"from"`
	ReplyTo   *Message    `json:"reply_to_message"`
}

// Document is an incoming file attachment (a subset of Telegram's Document). The
// dispatcher resolves FileID to a download via getFile and lands the bytes in
// the responder's outbox; FileSize gates the download against the size cap.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// PhotoSize is one rendition of an incoming photo — Telegram sends the same
// image at several resolutions. A photo carries no file name or MIME type (it is
// always JPEG), so those are synthesized when it is fetched.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// largestPhoto returns the highest-resolution rendition (Telegram orders them
// ascending, but pick by byte size, then by pixel area, rather than trust the
// order), or nil for an empty set.
func largestPhoto(sizes []PhotoSize) *PhotoSize {
	var best *PhotoSize
	for i := range sizes {
		s := &sizes[i]
		if best == nil || s.FileSize > best.FileSize ||
			(s.FileSize == best.FileSize && s.Width*s.Height > best.Width*best.Height) {
			best = s
		}
	}
	return best
}

// Chat identifies the conversation an update belongs to.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
	// Title is the group/supergroup/channel display name — present on every group
	// update, absent in a private chat. It is what names a GROUP transcript's
	// meta.json (a group chat has no single person to identify it).
	Title string `json:"title"`
	// Username is the public @handle (without @) of a public group/supergroup/channel
	// — absent for a private group and for private chats. Recorded alongside Title so a
	// public group is addressable by its handle.
	Username string `json:"username"`
}

// isGroup reports whether the chat is a (super)group. An empty Type — the zero
// value Telegram never actually sends, but tests and channel posts leave unset —
// is treated as NOT a group, so the private path is the safe default.
func (c Chat) isGroup() bool { return c.Type == "group" || c.Type == "supergroup" }

// User is the sender of a message. id/username are used for logging and access
// control; first_name feeds the transcript store's per-chat meta.json (so the owner
// can tell a numeric chat_id apart from a person).
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
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
	return decodeEnvelope[[]Update](status, body)
}

// fileURL builds the download URL for a getFile file_path. Note the /file/
// segment: downloaded files live at <base>/file/bot<token>/<path>, NOT under the
// method URL. Like methodURL it embeds the token — so DownloadFile scrubs its
// transport errors.
func (c *Client) fileURL(filePath string) string {
	return fmt.Sprintf("%s/file/bot%s/%s", c.baseURL(), c.Token, filePath)
}

// GetFile resolves a file_id to a downloadable file_path (getFile). The bot API
// only serves files up to ~20 MB this way; a larger file_id yields an APIError.
func (c *Client) GetFile(ctx context.Context, fileID string) (string, error) {
	status, body, err := c.postJSON(ctx, "getFile", map[string]any{"file_id": fileID})
	if err != nil {
		return "", err
	}
	res, err := decodeEnvelope[struct {
		FilePath string `json:"file_path"`
	}](status, body)
	if err != nil {
		return "", err
	}
	if res.FilePath == "" {
		return "", fmt.Errorf("getFile (HTTP %d): empty file_path", status)
	}
	return res.FilePath, nil
}

// DownloadFile streams the file at a getFile file_path into dst and returns the
// number of bytes written. When limit > 0 it copies at most limit bytes (the
// caller passes cap+1 and treats an over-limit result as oversized, so a file
// whose declared size lied cannot fill the disk). Transport errors are scrubbed
// of the token (which the file URL embeds).
func (c *Client) DownloadFile(ctx context.Context, filePath string, dst io.Writer, limit int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL(filePath), nil)
	if err != nil {
		return 0, c.scrubToken(err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, c.scrubToken(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("downloading file (HTTP %d)", resp.StatusCode)
	}
	src := io.Reader(resp.Body)
	if limit > 0 {
		src = io.LimitReader(resp.Body, limit)
	}
	return io.Copy(dst, src)
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

// BotCommandScope narrows a setMyCommands upload to a subset of chats — e.g.
// {Type:"all_group_chats"} for a menu shown only in groups. A nil scope uploads
// to the default scope. See the Bot API BotCommandScope types.
type BotCommandScope struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id,omitempty"`
}

// SetMyCommands uploads the bot's command menu (the "/" list clients show),
// replacing any previously set commands for the given scope (nil => default
// scope). Best-effort at startup — the bot works without a menu, so the caller
// may just log a failure.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand, scope *BotCommandScope) error {
	payload := map[string]any{"commands": commands}
	if scope != nil {
		payload["scope"] = scope
	}
	status, body, err := c.postJSON(ctx, "setMyCommands", payload)
	if err != nil {
		return err
	}
	return checkOK(status, body)
}

// GetMe returns the bot's own account (getMe). Used at startup to learn the
// bot's @username, which @mention addressing in groups matches against. An empty
// payload object — getMe takes no arguments.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	status, body, err := c.postJSON(ctx, "getMe", map[string]any{})
	if err != nil {
		return nil, err
	}
	u, err := decodeEnvelope[User](status, body)
	if err != nil {
		return nil, err
	}
	return &u, nil
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
	// The filename lands in the part's Content-Disposition header. mime/multipart
	// only escapes " and \, so a CR/LF would pass through and could inject a second
	// form field (e.g. a chat_id retargeting the route). The name is model- or
	// user-supplied (a send_document display name, a spilled snippet name, an
	// echoed incoming name), so strip control chars here, at the sink.
	part, err := mw.CreateFormFile("document", stripControl(filename))
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
		return 0, c.scrubToken(err)
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
		return 0, nil, c.scrubToken(err)
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
		return 0, nil, c.scrubToken(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// scrubbedError hides the bot token in a transport error's message while
// preserving the underlying error for errors.Is/As traversal (e.g. matching
// context.Canceled). net/http returns a *url.Error for a failed request, and
// its Error() embeds the full request URL — which contains /bot<token>/.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubToken redacts the bot token from an error whose text may embed it. The
// token lives in every request URL (methodURL), so both a transport failure and
// a URL-parse failure carry it; either would otherwise reach the logs verbatim
// (loudest in the getUpdates long-poll loop, which retries on every failure).
// This is the single choke point — every Bot API call routes its request
// construction and transport errors through here. Errors that cannot contain
// the token (JSON, file, decode, structured APIError) pass through untouched.
func (c *Client) scrubToken(err error) error {
	if err == nil || c.Token == "" {
		return err
	}
	msg := err.Error()
	scrubbed := strings.ReplaceAll(msg, c.Token, "<redacted>")
	if scrubbed == msg {
		return err
	}
	return &scrubbedError{msg: scrubbed, err: err}
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

// decodeEnvelope decodes a Bot API {ok, error_code, description, parameters,
// result} envelope and returns the result payload (T is the method's result
// shape). Every Bot API method funnels through here, so a non-OK envelope
// uniformly becomes a structured *APIError — the dispatcher's errors.As
// classification works on any method's failure, getUpdates included.
func decodeEnvelope[T any](status int, body []byte) (T, error) {
	var ar struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  *apiErrorParams `json:"parameters"`
		Result      T               `json:"result"`
	}
	var zero T
	if err := json.Unmarshal(body, &ar); err != nil {
		return zero, fmt.Errorf("decoding Telegram response (HTTP %d): %w", status, err)
	}
	if !ar.OK {
		return zero, newAPIError(status, ar.ErrorCode, ar.Description, ar.Parameters)
	}
	return ar.Result, nil
}

// checkOK verifies a Bot API response envelope carries ok:true (for methods
// whose result payload the caller does not need, e.g. sendChatAction).
func checkOK(status int, body []byte) error {
	_, err := decodeEnvelope[json.RawMessage](status, body)
	return err
}

// decodeMessageID unwraps the {ok, result:{message_id}} envelope.
func decodeMessageID(status int, body []byte) (int64, error) {
	res, err := decodeEnvelope[struct {
		MessageID int64 `json:"message_id"`
	}](status, body)
	if err != nil {
		return 0, err
	}
	return res.MessageID, nil
}
