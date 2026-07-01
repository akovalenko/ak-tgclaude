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
	mu      sync.Mutex
	calls   []fakeCall
	actions []string // recorded SendChatAction actions
	err     error    // if set, every call fails with it
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

func (f *fakeSender) SendChatAction(_ context.Context, _ int64, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.actions = append(f.actions, action)
	return nil
}

func (f *fakeSender) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeSender) actionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actions)
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
	if _, err := drainExisting(context.Background(), dir, Route{ChatID: 9}, f, map[string]int{}); err != nil {
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
	if _, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{}); err != nil {
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
	if _, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{}); err != nil {
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
	// A network error is transient: the pass stops (head-of-line) and asks for a
	// retry, but is not itself a fatal drain error.
	f := &fakeSender{err: errors.New("network down")}
	retryIn, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{})
	if err != nil {
		t.Fatalf("transient failure should not be a fatal error: %v", err)
	}
	if retryIn <= 0 {
		t.Errorf("transient failure should schedule a retry, got %v", retryIn)
	}
	// Both descriptors must remain (head-of-line: nothing acked).
	if left := remainingJSON(t, dir); len(left) != 2 {
		t.Errorf("descriptors should be retained on send failure, have %v", left)
	}
}

// scriptedSender returns a preset outcome per successive Send* call (errs[i] for
// the i-th call, nil = success), letting a test drive a mixed
// permanent/success/transient sequence through drainExisting.
type scriptedSender struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *scriptedSender) next() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return 0, s.errs[i]
	}
	return int64(s.calls), nil
}

func (s *scriptedSender) SendMessage(context.Context, Route, string, string, bool) (int64, error) {
	return s.next()
}

func (s *scriptedSender) SendDocument(context.Context, Route, string, string, string, string, bool) (int64, error) {
	return s.next()
}

func (s *scriptedSender) SendChatAction(context.Context, int64, string) error { return nil }

func (s *scriptedSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func badDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, "bad"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		permanent  bool
		retryAfter time.Duration
	}{
		{"400 bad request", &APIError{Code: 400}, true, 0},
		{"403 blocked", &APIError{Code: 403}, true, 0},
		{"404 chat not found", &APIError{Code: 404}, true, 0},
		{"429 rate limit", &APIError{Code: 429, RetryAfter: 7}, false, 7 * time.Second},
		{"500 server error", &APIError{Code: 500}, false, 0},
		{"network error", errors.New("connection refused"), false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			perm, ra := classify(c.err)
			if perm != c.permanent || ra != c.retryAfter {
				t.Errorf("classify = (%v, %v), want (%v, %v)", perm, ra, c.permanent, c.retryAfter)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	if got := backoff(1); got != baseBackoff {
		t.Errorf("backoff(1) = %v, want %v", got, baseBackoff)
	}
	if got := backoff(0); got != baseBackoff {
		t.Errorf("backoff(0) should clamp to base, got %v", got)
	}
	// Non-decreasing and never above the cap.
	prev := time.Duration(0)
	for a := 1; a <= 20; a++ {
		d := backoff(a)
		if d < prev {
			t.Errorf("backoff not monotonic at %d: %v < %v", a, d, prev)
		}
		if d > maxBackoff {
			t.Errorf("backoff(%d) = %v exceeds cap %v", a, d, maxBackoff)
		}
		prev = d
	}
	if got := backoff(100); got != maxBackoff {
		t.Errorf("backoff(100) = %v, want cap %v", got, maxBackoff)
	}
}

func TestDrainQuarantinesPermanentReject(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if _, err := (&Descriptor{Kind: KindText, Text: "m"}).Drop(dir); err != nil {
			t.Fatal(err)
		}
	}
	// First send is a permanent 400; the second succeeds. The wedge fix means the
	// pass continues past the bad head and delivers the second in the SAME pass.
	f := &scriptedSender{errs: []error{&APIError{Code: 400, Description: "bad request"}}}
	retryIn, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{})
	if err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	if retryIn != 0 {
		t.Errorf("permanent reject must not schedule a retry, got %v", retryIn)
	}
	if f.callCount() != 2 {
		t.Fatalf("queue wedged: want 2 send attempts, got %d", f.callCount())
	}
	if left := remainingJSON(t, dir); len(left) != 0 {
		t.Errorf("both descriptors should leave the active spool, have %v", left)
	}
	if bad := badDir(t, dir); len(bad) != 1 {
		t.Errorf("permanent reject should be quarantined in bad/, have %v", bad)
	}
}

func TestDrainBacksOffTransient(t *testing.T) {
	dir := t.TempDir()
	if _, err := (&Descriptor{Kind: KindText, Text: "m"}).Drop(dir); err != nil {
		t.Fatal(err)
	}
	// Every send fails 500 (transient): retried until maxSendAttempts, then given up.
	f := &fakeSender{err: &APIError{Code: 500}}
	attempts := map[string]int{}
	var lastRetry time.Duration
	for i := 0; i < maxSendAttempts; i++ {
		retryIn, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, attempts)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		lastRetry = retryIn
		if i < maxSendAttempts-1 {
			if retryIn <= 0 {
				t.Errorf("pass %d: want a positive back-off, got %v", i, retryIn)
			}
			if left := remainingJSON(t, dir); len(left) != 1 {
				t.Errorf("pass %d: descriptor should be retained, have %v", i, left)
			}
		}
	}
	if lastRetry != 0 {
		t.Errorf("give-up should not re-arm the retry timer, got %v", lastRetry)
	}
	if left := remainingJSON(t, dir); len(left) != 0 {
		t.Errorf("given-up descriptor should be quarantined, still present: %v", left)
	}
	if bad := badDir(t, dir); len(bad) != 1 {
		t.Errorf("given-up descriptor should be in bad/, have %v", bad)
	}
}

func TestServeOutboxWatchDelivers(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeSender{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); serveOutbox(ctx, dir, Route{ChatID: 1}, f, stop) }()

	// Give the watcher a moment to register, then drop a descriptor.
	time.Sleep(100 * time.Millisecond)
	if _, err := (&Descriptor{Kind: KindText, Text: "live"}).Drop(dir); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.snapshot()) == 1 {
			close(stop)
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watched drop was not delivered within deadline")
}

func TestServeOutboxFinalFlush(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSender{}
	stop := make(chan struct{})
	done := make(chan struct{})
	// Drop BEFORE starting, then stop immediately: the final flush (or catch-up)
	// must still deliver it.
	if _, err := (&Descriptor{Kind: KindText, Text: "queued"}).Drop(dir); err != nil {
		t.Fatal(err)
	}
	go func() { defer close(done); serveOutbox(context.Background(), dir, Route{ChatID: 1}, f, stop) }()
	close(stop)
	<-done
	if calls := f.snapshot(); len(calls) != 1 || calls[0].text != "queued" {
		t.Errorf("queued descriptor not flushed: %+v", calls)
	}
}
