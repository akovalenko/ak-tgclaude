package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSendText(t *testing.T) {
	d, outbox, err := parseSendText([]string{"--html", "--outbox", "/box", "<b>hi</b>"})
	if err != nil {
		t.Fatalf("parseSendText: %v", err)
	}
	if d.Kind != KindText || d.Text != "<b>hi</b>" || d.Format != FormatHTML {
		t.Errorf("unexpected descriptor: %+v", d)
	}
	if outbox != "/box" {
		t.Errorf("outbox = %q, want /box", outbox)
	}
}

func TestParseSendCode(t *testing.T) {
	d, _, err := parseSendCode([]string{"--lang", "go", "--caption", "main.go", "package main"})
	if err != nil {
		t.Fatalf("parseSendCode: %v", err)
	}
	if d.Kind != KindCode || d.Language != "go" || d.Caption != "main.go" || d.Code != "package main" {
		t.Errorf("unexpected descriptor: %+v", d)
	}
}

func TestParseSendDocStatsAndAbsolutizes(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "r.pdf")
	if err := os.WriteFile(rel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, _, err := parseSendDoc([]string{rel})
	if err != nil {
		t.Fatalf("parseSendDoc: %v", err)
	}
	if !filepath.IsAbs(d.Path) {
		t.Errorf("path %q is not absolute", d.Path)
	}

	if _, _, err := parseSendDoc([]string{filepath.Join(dir, "missing.pdf")}); err == nil {
		t.Errorf("expected error for missing attachment")
	}
	if _, _, err := parseSendDoc([]string{dir}); err == nil {
		t.Errorf("expected error for directory attachment")
	}
}

func TestParseSendTextFromFile(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "reply.html")
	if err := os.WriteFile(body, []byte("<b>Yes!</b> \"quoted\""), 0o600); err != nil {
		t.Fatal(err)
	}
	// --file keeps message text (with ! and quotes) out of argv.
	d, _, err := parseSendText([]string{"--html", "--file", body})
	if err != nil {
		t.Fatalf("parseSendText --file: %v", err)
	}
	if d.Text != "<b>Yes!</b> \"quoted\"" || d.Format != FormatHTML {
		t.Errorf("descriptor from file wrong: %+v", d)
	}

	if _, _, err := parseSendText([]string{"--file", filepath.Join(dir, "missing")}); err == nil {
		t.Errorf("missing body file should error")
	}
}

func TestResolveOutbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(outboxEnv, "")

	if _, err := resolveOutbox(""); err == nil {
		t.Errorf("expected error when no outbox set")
	}
	if got, err := resolveOutbox(dir); err != nil || got != dir {
		t.Errorf("resolveOutbox(%q) = %q, %v", dir, got, err)
	}

	t.Setenv(outboxEnv, dir)
	if got, err := resolveOutbox(""); err != nil || got != dir {
		t.Errorf("resolveOutbox from env = %q, %v", got, err)
	}

	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOutbox(file); err == nil {
		t.Errorf("expected error when outbox is a file")
	}
}

func TestWaitForResultFound(t *testing.T) {
	dir := t.TempDir()
	base := "00000000000000000001-1-00000000000000000001.json"
	if err := writeResult(dir, base, Result{OK: true, MessageID: 9}); err != nil {
		t.Fatal(err)
	}
	res, ok := waitForResult(dir, base, resultWaitTimeout)
	if !ok {
		t.Fatalf("waitForResult: result not found")
	}
	if !res.OK || res.MessageID != 9 {
		t.Errorf("result = %+v, want OK with message_id 9", res)
	}
}

func TestWaitForResultTimeout(t *testing.T) {
	dir := t.TempDir()
	res, ok := waitForResult(dir, "nope.json", 10*time.Millisecond)
	if ok || res != nil {
		t.Errorf("want (nil, false) on timeout, got (%+v, %v)", res, ok)
	}
}

func TestReportResult(t *testing.T) {
	var buf bytes.Buffer

	// Success is silent and exits 0.
	if code := reportResult(&buf, &Result{OK: true, MessageID: 1}, false); code != 0 {
		t.Errorf("success code = %d, want 0", code)
	}
	if buf.Len() != 0 {
		t.Errorf("success should be silent, got %q", buf.String())
	}

	// A permanent reject exits non-zero and surfaces the Telegram description.
	buf.Reset()
	if code := reportResult(&buf, &Result{OK: false, Permanent: true, Error: "Bad Request: can't parse entities"}, false); code == 0 {
		t.Errorf("permanent reject should exit non-zero")
	}
	if !strings.Contains(buf.String(), "can't parse entities") {
		t.Errorf("permanent output should carry the description, got %q", buf.String())
	}

	// A give-up exits non-zero.
	buf.Reset()
	if code := reportResult(&buf, &Result{OK: false, Permanent: false, Error: "network down"}, false); code == 0 {
		t.Errorf("give-up should exit non-zero")
	}

	// A timeout degrades to fire-and-forget: exit 0 with a "queued" note.
	buf.Reset()
	if code := reportResult(&buf, nil, true); code != 0 {
		t.Errorf("timeout code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "queued") {
		t.Errorf("timeout output should mention queued, got %q", buf.String())
	}
}
