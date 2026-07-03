package main

import (
	"context"
	"sync"
)

// fakeCall records one delivery a fakeSender received, for assertions.
type fakeCall struct {
	kind     string // "message" | "document"
	text     string
	mode     string
	filename string
	route    Route
}

// fakeSender is an in-memory Sender for tests: it records calls (and chat
// actions) instead of talking to Telegram, and can be made to fail every call.
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
