package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCall struct {
	kind     string // "message" | "document"
	text     string
	mode     string
	filename string
	route    Route
}

type fakeSender struct {
	mu    sync.Mutex
	calls []fakeCall
	err   error // if set, every call fails with it
}

func (f *fakeSender) SendMessage(_ context.Context, r Route, text, mode string, _ bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.calls = append(f.calls, fakeCall{kind: "message", text: text, mode: mode, route: r})
	return int64(len(f.calls)), nil
}

func (f *fakeSender) SendDocument(_ context.Context, r Route, _, filename, _, _ string, _ bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.calls = append(f.calls, fakeCall{kind: "document", filename: filename, route: r})
	return int64(len(f.calls)), nil
}

func (f *fakeSender) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func remainingJSON(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && isDescriptor(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestDrainExistingOrderAndRemoval(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []*Descriptor{
		{Kind: KindText, Text: "first"},
		{Kind: KindCode, Code: "second", Language: "go"},
		{Kind: KindDocument, Path: "/abs/x.pdf", Filename: "x.pdf"},
	} {
		if _, err := d.Drop(dir); err != nil {
			t.Fatal(err)
		}
	}

	f := &fakeSender{}
	if err := drainExisting(context.Background(), dir, Route{ChatID: 9}, f); err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	calls := f.snapshot()
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
	}
	if calls[0].text != "first" || calls[1].kind != "message" || calls[2].kind != "document" {
		t.Errorf("order/kind wrong: %+v", calls)
	}
	if !strings.Contains(calls[1].text, `class="language-go"`) {
		t.Errorf("code not rendered: %q", calls[1].text)
	}
	if calls[2].filename != "x.pdf" || calls[0].route.ChatID != 9 {
		t.Errorf("doc/route wrong: %+v", calls)
	}
	if left := remainingJSON(t, dir); len(left) != 0 {
		t.Errorf("descriptors not removed: %v", left)
	}
}

func TestDrainSpillsOversizedCode(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", telegramTextLimit+10)
	if _, err := (&Descriptor{Kind: KindCode, Code: big, Language: "go"}).Drop(dir); err != nil {
		t.Fatal(err)
	}
	f := &fakeSender{}
	if err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f); err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].kind != "document" || calls[0].filename != "snippet.go" {
		t.Errorf("oversized code should spill to snippet.go document: %+v", calls)
	}
}

func TestDrainQuarantinesBadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid one after it must still go through.
	if _, err := (&Descriptor{Kind: KindText, Text: "ok"}).Drop(dir); err != nil {
		t.Fatal(err)
	}

	f := &fakeSender{}
	if err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f); err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	if calls := f.snapshot(); len(calls) != 1 || calls[0].text != "ok" {
		t.Errorf("valid descriptor should still send: %+v", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad", "000-bad.json")); err != nil {
		t.Errorf("bad descriptor not quarantined: %v", err)
	}
}

func TestDrainStopsOnSendErrorPreservingOrder(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if _, err := (&Descriptor{Kind: KindText, Text: "m"}).Drop(dir); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeSender{err: errors.New("network down")}
	err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f)
	if err == nil {
		t.Fatalf("expected send error to propagate")
	}
	// Both descriptors must remain (head-of-line: nothing acked).
	if left := remainingJSON(t, dir); len(left) != 2 {
		t.Errorf("descriptors should be retained on send failure, have %v", left)
	}
}

func TestDrainOutboxWatchDelivers(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeSender{}
	go DrainOutbox(ctx, dir, Route{ChatID: 1}, f)

	// Give the watcher a moment to register, then drop a descriptor.
	time.Sleep(100 * time.Millisecond)
	if _, err := (&Descriptor{Kind: KindText, Text: "live"}).Drop(dir); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.snapshot()) == 1 {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watched drop was not delivered within deadline")
}
