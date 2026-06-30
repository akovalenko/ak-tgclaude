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
