package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	// Note: on Linux `\` is not a path separator, so a Windows-style path is not
	// split by filepath.Base; the separator-safety loop below covers those (the
	// backslashes are neutralized to `_`, which is still traversal-safe).
	cases := map[string]string{
		"report.pdf":       "report.pdf",
		"../../etc/passwd": "passwd", // traversal stripped to the basename
		"a/b/c.txt":        "c.txt",
		"  spaced.txt  ":   "spaced.txt",
		".":                "",
		"..":               "",
		"/":                "",
		"":                 "",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever survives must never contain a path separator.
	for _, in := range []string{"../../x", "a/b", `c\d`, "..\\..\\e"} {
		if s := sanitizeFilename(in); strings.ContainsAny(s, `/\`) {
			t.Errorf("sanitizeFilename(%q) = %q still has a separator", in, s)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:               "0 B",
		512:             "512 B",
		1024:            "1.0 KB",
		1536:            "1.5 KB",
		1024 * 1024:     "1.0 MB",
		3 * 1024 * 1024: "3.0 MB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// docServer stands up a fake Bot API serving getFile + the file download, and
// returns a Client and Dispatcher wired to it.
func docServer(t *testing.T, content string, cap int64) (*Dispatcher, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file_1.bin"}}`))
		case strings.Contains(r.URL.Path, "/file/"):
			_, _ = w.Write([]byte(content))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
	}))
	c := &Client{Token: "TOK", BaseURL: srv.URL, HTTP: srv.Client()}
	d := &Dispatcher{client: c, maxIncomingBytes: cap}
	return d, srv.Close
}

func TestFetchIncomingDocument(t *testing.T) {
	const content = "the file contents"
	d, closeSrv := docServer(t, content, 20<<20)
	defer closeSrv()

	docDir := t.TempDir()
	m := &Message{
		MessageID: 42,
		Document:  &Document{FileID: "ABC", FileName: "notes.txt", MimeType: "text/plain", FileSize: int64(len(content))},
	}
	att, err := d.fetchIncomingDocument(context.Background(), m, docDir)
	if err != nil {
		t.Fatalf("fetchIncomingDocument: %v", err)
	}
	// Lands under incoming/ with the msgid prefix.
	want := filepath.Join(docDir, "incoming", "42-notes.txt")
	if att.Path != want {
		t.Errorf("path = %q, want %q", att.Path, want)
	}
	got, err := os.ReadFile(att.Path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != content {
		t.Errorf("saved content = %q, want %q", got, content)
	}
	if att.Filename != "notes.txt" || att.MimeType != "text/plain" || att.Size != int64(len(content)) {
		t.Errorf("attachment metadata wrong: %+v", att)
	}
}

func TestFetchIncomingDocumentTraversalName(t *testing.T) {
	d, closeSrv := docServer(t, "x", 20<<20)
	defer closeSrv()

	docDir := t.TempDir()
	m := &Message{MessageID: 7, Document: &Document{FileID: "ABC", FileName: "../../escape.sh"}}
	att, err := d.fetchIncomingDocument(context.Background(), m, docDir)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// The saved path stays strictly under the incoming dir — no escape.
	incoming := filepath.Join(docDir, "incoming")
	if !strings.HasPrefix(att.Path, incoming+string(filepath.Separator)) {
		t.Errorf("saved outside incoming dir: %q", att.Path)
	}
	if att.Path != filepath.Join(incoming, "7-escape.sh") {
		t.Errorf("path = %q, want .../7-escape.sh", att.Path)
	}
}

func TestFetchIncomingDocumentOverLimit(t *testing.T) {
	// file_size lies (says small) but the body is 100 bytes; cap is 10.
	d, closeSrv := docServer(t, strings.Repeat("A", 100), 10)
	defer closeSrv()

	docDir := t.TempDir()
	m := &Message{MessageID: 1, Document: &Document{FileID: "ABC", FileName: "big.bin", FileSize: 5}}
	if _, err := d.fetchIncomingDocument(context.Background(), m, docDir); err == nil {
		t.Fatal("want an over-limit error for a lying file_size")
	}
	// The partial download must have been cleaned up.
	if entries, _ := os.ReadDir(filepath.Join(docDir, "incoming")); len(entries) != 0 {
		t.Errorf("over-limit download left files behind: %v", entries)
	}
}
