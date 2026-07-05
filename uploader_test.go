package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeScript writes an executable /bin/sh script (body appended after the
// shebang) into a temp dir and returns its path — a stand-in uploader for tests.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "up.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewUploader(t *testing.T) {
	if newUploader("", 40, 300) != nil {
		t.Error("no command => nil (feature off)")
	}
	u := newUploader("cmd", 40, 300)
	if u.thresholdBytes != 40<<20 {
		t.Errorf("thresholdBytes = %d, want %d", u.thresholdBytes, 40<<20)
	}
	// Hard cap sits 10% above the advertised 300 → 330 MB.
	if u.hardCapBytes != 330<<20 {
		t.Errorf("hardCapBytes = %d, want %d", u.hardCapBytes, 330<<20)
	}
	if u2 := newUploader("cmd", 40, 0); u2.hardCapBytes != 0 {
		t.Errorf("max 0 => no hard cap, got %d", u2.hardCapBytes)
	}
}

func TestSuggestName(t *testing.T) {
	// A random 8-hex prefix joined to the original name (extension preserved whole).
	got := suggestName("dist.tar.gz")
	if !regexp.MustCompile(`^[0-9a-f]{8}-dist\.tar\.gz$`).MatchString(got) {
		t.Errorf("suggestName = %q, want <8hex>-dist.tar.gz", got)
	}
}

func TestUploaderDeliverSuccess(t *testing.T) {
	// The script echoes arg2 (the suggested name) into the URL — so a passing test
	// proves both that arg2 is passed and that stdout's first line becomes the URL.
	cmd := writeScript(t, `echo "https://murphy/share/$2"`)
	file := filepath.Join(t.TempDir(), "dist.tar.gz")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &uploader{command: cmd, thresholdBytes: 0}
	f := &fakeSender{}
	d := &Descriptor{Kind: KindDocument, Path: file, Filename: "dist.tar.gz", Caption: "here"}
	if _, err := u.deliver(context.Background(), d, Route{ChatID: 7}, f, 100); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].kind != "message" {
		t.Fatalf("want 1 message, got %+v", calls)
	}
	text := calls[0].text
	if !strings.HasPrefix(text, "here\ndist.tar.gz\nhttps://murphy/share/") {
		t.Errorf("caption+name+url layout wrong: %q", text)
	}
	if !strings.HasSuffix(text, "-dist.tar.gz") {
		t.Errorf("arg2 suggested name not applied by the uploader: %q", text)
	}
	if calls[0].route.ChatID != 7 {
		t.Errorf("route not passed through: %+v", calls[0].route)
	}
}

func TestUploaderDeliverTooBig(t *testing.T) {
	// Over the hard cap → rejected with the clear error BEFORE the uploader runs.
	u := &uploader{command: "/definitely/not/run", hardCapBytes: 100 << 20}
	f := &fakeSender{}
	d := &Descriptor{Kind: KindDocument, Path: "x", Filename: "big.bin"}
	_, err := u.deliver(context.Background(), d, Route{ChatID: 1}, f, 200<<20)
	var ue *uploadError
	if !errors.As(err, &ue) {
		t.Fatalf("want *uploadError, got %v", err)
	}
	if !strings.Contains(ue.Error(), "слишком большой") {
		t.Errorf("error message = %q", ue.Error())
	}
	if len(f.snapshot()) != 0 {
		t.Errorf("nothing must be sent when over the cap")
	}
}

func TestUploaderDeliverRejectsUnsafeName(t *testing.T) {
	// A shell-dangerous name is refused BEFORE the uploader runs, whether it rides
	// the display Filename or the outbox file's own basename (both reach the script).
	cases := []struct {
		name, filename, path string
	}{
		{"backtick display name", "file`rm -rf`.txt", "/out/safe.txt"},
		{"dollar display name", "x$(id).bin", "/out/safe.bin"},
		{"dangerous outbox basename", "safe.txt", "/out/evil`whoami`.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &uploader{command: "/definitely/not/run"}
			f := &fakeSender{}
			d := &Descriptor{Kind: KindDocument, Path: tc.path, Filename: tc.filename}
			_, err := u.deliver(context.Background(), d, Route{ChatID: 1}, f, 10)
			var ue *uploadError
			if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "sane file name") {
				t.Fatalf("want a sane-name rejection, got %v", err)
			}
			if len(f.snapshot()) != 0 {
				t.Errorf("nothing must be sent when the name is rejected")
			}
		})
	}
	// Spaces and non-ASCII are fine — the uploader is expected to quote them.
	if !uploadNameOK("Отчёт за июль 2026.pdf") {
		t.Errorf("a spaced, non-ASCII name should be allowed")
	}
	if uploadNameOK("a;b.txt") || uploadNameOK("a|b.txt") || uploadNameOK("a\nb.txt") {
		t.Errorf("shell metacharacters / newlines must be rejected")
	}
}

func TestUploaderRunFailure(t *testing.T) {
	cmd := writeScript(t, `echo "boom" >&2; exit 3`)
	u := &uploader{command: cmd, thresholdBytes: 0}
	f := &fakeSender{}
	d := &Descriptor{Kind: KindDocument, Path: "f", Filename: "f"}
	_, err := u.deliver(context.Background(), d, Route{ChatID: 1}, f, 10)
	var ue *uploadError
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "boom") {
		t.Fatalf("want upload failed with stderr tail, got %v", err)
	}
	if len(f.snapshot()) != 0 {
		t.Errorf("no send on uploader failure")
	}
}

func TestUploaderNoURL(t *testing.T) {
	cmd := writeScript(t, `exit 0`) // succeeds but prints nothing
	u := &uploader{command: cmd, thresholdBytes: 0}
	f := &fakeSender{}
	d := &Descriptor{Kind: KindDocument, Path: "f", Filename: "f"}
	_, err := u.deliver(context.Background(), d, Route{ChatID: 1}, f, 10)
	var ue *uploadError
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "no URL") {
		t.Fatalf("want no-URL error, got %v", err)
	}
}

func TestSendDescriptorRoutesBigDocument(t *testing.T) {
	cmd := writeScript(t, `echo "https://h/$2"`)
	file := filepath.Join(t.TempDir(), "r.pdf")
	if err := os.WriteFile(file, []byte("12345"), 0o600); err != nil { // 5 bytes
		t.Fatal(err)
	}
	d := &Descriptor{Kind: KindDocument, Path: file, Filename: "r.pdf"}

	// Over threshold (0 => any non-empty file) → uploaded, delivered as a message.
	f := &fakeSender{}
	if _, err := sendDescriptor(context.Background(), d, Route{ChatID: 1}, f, &uploader{command: cmd, thresholdBytes: 0}, overflowSpill); err != nil {
		t.Fatal(err)
	}
	if c := f.snapshot(); len(c) != 1 || c[0].kind != "message" {
		t.Fatalf("big doc should upload → message, got %+v", c)
	}

	// At/below threshold → normal Telegram attachment.
	f2 := &fakeSender{}
	if _, err := sendDescriptor(context.Background(), d, Route{ChatID: 1}, f2, &uploader{command: cmd, thresholdBytes: 1 << 20}, overflowSpill); err != nil {
		t.Fatal(err)
	}
	if c := f2.snapshot(); len(c) != 1 || c[0].kind != "document" {
		t.Fatalf("small doc should attach → document, got %+v", c)
	}
}
