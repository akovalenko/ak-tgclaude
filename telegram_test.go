package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientSendMessage(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer srv.Close()

	c := &Client{Token: "TESTTOKEN", BaseURL: srv.URL, HTTP: srv.Client()}
	id, err := c.SendMessage(context.Background(), Route{ChatID: 100, ReplyTo: 7}, "<b>hi</b>", "HTML", true)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != 42 {
		t.Errorf("message_id = %d, want 42", id)
	}
	if gotPath != "/botTESTTOKEN/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if body["chat_id"].(float64) != 100 || body["text"] != "<b>hi</b>" || body["parse_mode"] != "HTML" {
		t.Errorf("payload = %v", body)
	}
	if body["reply_to_message_id"].(float64) != 7 || body["disable_notification"] != true {
		t.Errorf("reply/silent not set: %v", body)
	}
}

func TestClientSendMessagePlainOmitsParseMode(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.SendMessage(context.Background(), Route{ChatID: 1}, "hi", "", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["parse_mode"]; ok {
		t.Errorf("parse_mode should be omitted for plain, got %v", body["parse_mode"])
	}
	if _, ok := body["reply_to_message_id"]; ok {
		t.Errorf("reply_to should be omitted when 0")
	}
}

func TestClientSendChatAction(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.SendChatAction(context.Background(), 100, "typing"); err != nil {
		t.Fatalf("SendChatAction: %v", err)
	}
	if gotPath != "/botT/sendChatAction" {
		t.Errorf("path = %q", gotPath)
	}
	if body["chat_id"].(float64) != 100 || body["action"] != "typing" {
		t.Errorf("payload = %v", body)
	}
}

func TestClientSendChatActionAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	err := c.SendChatAction(context.Background(), 100, "typing")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("expected API error surfaced, got %v", err)
	}
}

func TestClientSetMyCommands(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	cmds := []BotCommand{{Command: "help", Description: "help"}, {Command: "clear", Description: "reset"}}
	if err := c.SetMyCommands(context.Background(), cmds); err != nil {
		t.Fatalf("SetMyCommands: %v", err)
	}
	if gotPath != "/botT/setMyCommands" {
		t.Errorf("path = %q", gotPath)
	}
	list, ok := body["commands"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("commands payload = %v", body["commands"])
	}
	first := list[0].(map[string]any)
	if first["command"] != "help" || first["description"] != "help" {
		t.Errorf("first command = %v", first)
	}
}

func TestClientSendDocument(t *testing.T) {
	var fileContent, fileName, chatID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		chatID = r.FormValue("chat_id")
		f, hdr, err := r.FormFile("document")
		if err != nil {
			t.Errorf("FormFile: %v", err)
		} else {
			fileName = hdr.Filename
			b, _ := io.ReadAll(f)
			fileContent = string(b)
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(p, []byte("PDFBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	id, err := c.SendDocument(context.Background(), Route{ChatID: 55}, p, "report.pdf", "cap", "", false)
	if err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	if id != 7 {
		t.Errorf("message_id = %d, want 7", id)
	}
	if chatID != "55" || fileName != "report.pdf" || fileContent != "PDFBYTES" {
		t.Errorf("chat=%q name=%q content=%q", chatID, fileName, fileContent)
	}
}

func TestClientGetUpdates(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":[
			{"update_id":10,"message":{"message_id":5,"text":"hi","chat":{"id":99},"from":{"id":7,"username":"bob"}}},
			{"update_id":11,"message":{"message_id":6,"text":"yo","chat":{"id":99},"reply_to_message":{"message_id":5}}}
		]}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	ups, err := c.GetUpdates(context.Background(), 10, 30)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if body["offset"].(float64) != 10 || body["timeout"].(float64) != 30 {
		t.Errorf("request body wrong: %v", body)
	}
	if len(ups) != 2 {
		t.Fatalf("got %d updates, want 2", len(ups))
	}
	if ups[0].UpdateID != 10 || ups[0].Message.Text != "hi" || ups[0].Message.Chat.ID != 99 {
		t.Errorf("update 0 wrong: %+v", ups[0])
	}
	if ups[1].Message.ReplyTo == nil || ups[1].Message.ReplyTo.MessageID != 5 {
		t.Errorf("reply_to not parsed: %+v", ups[1].Message)
	}
	if ups[0].Message.From == nil || ups[0].Message.From.ID != 7 || ups[0].Message.From.Username != "bob" {
		t.Errorf("from not parsed: %+v", ups[0].Message.From)
	}
}

func TestClientGetUpdatesParsesDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":8,"date":1751648400,"caption":"look",
				"document":{"file_id":"ABC","file_name":"report.pdf","mime_type":"application/pdf","file_size":1234},
				"chat":{"id":99}}}
		]}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	ups, err := c.GetUpdates(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	m := ups[0].Message
	if m.Caption != "look" || m.Text != "" {
		t.Errorf("caption/text = %q/%q, want look/empty", m.Caption, m.Text)
	}
	if m.Document == nil {
		t.Fatal("document not parsed")
	}
	if m.Document.FileID != "ABC" || m.Document.FileName != "report.pdf" ||
		m.Document.MimeType != "application/pdf" || m.Document.FileSize != 1234 {
		t.Errorf("document fields wrong: %+v", m.Document)
	}
	if m.Date != 1751648400 {
		t.Errorf("date = %d, want 1751648400", m.Date)
	}
}

func TestClientGetFile(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"ok":true,"result":{"file_path":"documents/file_8.pdf"}}`)
	}))
	defer srv.Close()

	c := &Client{Token: "TOK", BaseURL: srv.URL, HTTP: srv.Client()}
	fp, err := c.GetFile(context.Background(), "ABC")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if fp != "documents/file_8.pdf" {
		t.Errorf("file_path = %q", fp)
	}
	if gotPath != "/botTOK/getFile" || body["file_id"] != "ABC" {
		t.Errorf("request wrong: path=%q body=%v", gotPath, body)
	}
}

func TestClientGetFileAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok":false,"error_code":400,"description":"file is too big"}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.GetFile(context.Background(), "ABC")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 400 {
		t.Fatalf("want *APIError code 400, got %v", err)
	}
}

func TestClientDownloadFile(t *testing.T) {
	const content = "hello attachment bytes"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, content)
	}))
	defer srv.Close()

	c := &Client{Token: "TOK", BaseURL: srv.URL, HTTP: srv.Client()}
	var buf bytes.Buffer
	n, err := c.DownloadFile(context.Background(), "documents/file_8.pdf", &buf, 0)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	// The download URL carries the /file/bot<token>/ prefix, not the method URL.
	if gotPath != "/file/botTOK/documents/file_8.pdf" {
		t.Errorf("download path = %q", gotPath)
	}
	if buf.String() != content || n != int64(len(content)) {
		t.Errorf("got %q (n=%d), want %q", buf.String(), n, content)
	}

	// A limit truncates the copy (belt-and-suspenders against a lying file_size).
	buf.Reset()
	n, err = c.DownloadFile(context.Background(), "x", &buf, 5)
	if err != nil {
		t.Fatalf("DownloadFile limited: %v", err)
	}
	if n != 5 || buf.String() != content[:5] {
		t.Errorf("limited copy = %q (n=%d), want %q", buf.String(), n, content[:5])
	}
}

func TestClientDownloadFileScrubsToken(t *testing.T) {
	const token = "999:AAEsecretdownloadtoken"
	boom := errors.New("dial tcp: connection refused")
	c := &Client{Token: token, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Transport: errTransport{err: boom}}}
	_, err := c.DownloadFile(context.Background(), "documents/x.pdf", io.Discard, 0)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in download error: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("scrubbing broke the download error chain")
	}
}

func TestUserLabel(t *testing.T) {
	if got := userLabel(nil); got != "?" {
		t.Errorf("nil user => %q", got)
	}
	if got := userLabel(&User{ID: 7}); got != "7" {
		t.Errorf("id-only => %q", got)
	}
	if got := userLabel(&User{ID: 7, Username: "bob"}); got != "7(@bob)" {
		t.Errorf("id+username => %q", got)
	}
}

func TestClientAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
	}))
	defer srv.Close()

	c := &Client{Token: "T", BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.SendMessage(context.Background(), Route{ChatID: 1}, "hi", "", false)
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err = %v, want it to carry the description", err)
	}
}

func TestDecodeAPIErrorFields(t *testing.T) {
	// 429 with an authoritative retry_after: Code from error_code, RetryAfter set.
	body := []byte(`{"ok":false,"error_code":429,"parameters":{"retry_after":7},"description":"Too Many Requests"}`)
	_, err := decodeMessageID(200, body)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != 429 || ae.RetryAfter != 7 {
		t.Errorf("code=%d retryAfter=%d, want 429/7", ae.Code, ae.RetryAfter)
	}
	if !strings.Contains(ae.Description, "Too Many Requests") {
		t.Errorf("description = %q", ae.Description)
	}

	// 400 without error_code: Code falls back to the HTTP status, RetryAfter stays 0.
	_, err = decodeMessageID(400, []byte(`{"ok":false,"description":"Bad Request"}`))
	if !errors.As(err, &ae) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != 400 || ae.RetryAfter != 0 {
		t.Errorf("code=%d retryAfter=%d, want 400/0", ae.Code, ae.RetryAfter)
	}
}

// errTransport fails every request the way a DNS/connection error does. net/http
// wraps the returned error in a *url.Error whose message embeds the full request
// URL — which contains /bot<token>/ — so it exercises the token-leak path.
type errTransport struct{ err error }

func (t errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func TestClientTransportErrorScrubsToken(t *testing.T) {
	const token = "123456789:AAErealLookingSecretTokenValue"
	boom := errors.New("dial tcp: connection refused")
	c := &Client{
		Token:   token,
		BaseURL: "https://api.telegram.org",
		HTTP:    &http.Client{Transport: errTransport{err: boom}},
	}

	// getUpdates is the loudest leak site (the long-poll loop logs every failure).
	_, err := c.GetUpdates(context.Background(), 0, 0)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in transport error: %v", err)
	}
	// The method context is still useful for debugging — only the token is gone.
	if !strings.Contains(err.Error(), "getUpdates") {
		t.Errorf("scrubbed error dropped method context: %v", err)
	}
	// Scrubbing must not sever the chain: errors.Is/As still have to traverse.
	if !errors.Is(err, boom) {
		t.Errorf("scrubbing broke the error chain: errors.Is(err, boom) = false")
	}
}
