package main

import (
	"os"
	"path/filepath"
	"testing"
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
