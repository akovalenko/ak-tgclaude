package main

import (
	"context"
	"encoding/json"
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
