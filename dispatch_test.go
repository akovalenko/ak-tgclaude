package main

import (
	"context"
	"testing"
)

// fakeResponder records the request it got and, simulating the agent's `send`
// calls, drops descriptors into the invocation's outbox.
type fakeResponder struct {
	sid         string
	descriptors []*Descriptor
	gotReq      RespondRequest
	called      bool
}

func (f *fakeResponder) Respond(_ context.Context, req RespondRequest) (RespondResult, error) {
	f.called = true
	f.gotReq = req
	for _, d := range f.descriptors {
		if _, err := d.Drop(req.OutboxDir); err != nil {
			return RespondResult{}, err
		}
	}
	return RespondResult{SessionID: f.sid}, nil
}

func newTestDispatcher(t *testing.T, resp Responder, sender Sender) *Dispatcher {
	t.Helper()
	store, err := LoadSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{
		sender:      sender,
		store:       store,
		resp:        resp,
		runtimeBase: t.TempDir(),
		pollTimeout: 1,
	}
}

func textUpdate(updateID, chatID, msgID int64, text string) Update {
	return Update{
		UpdateID: updateID,
		Message:  &Message{MessageID: msgID, Text: text, Chat: Chat{ID: chatID}},
	}
}

func TestHandleNewSessionBindsAndDelivers(t *testing.T) {
	resp := &fakeResponder{sid: "sess-1", descriptors: []*Descriptor{{Kind: KindText, Text: "answer"}}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hello"))

	if !resp.called || resp.gotReq.SessionID != "" || resp.gotReq.Prompt != "hello" {
		t.Errorf("responder req wrong: called=%v req=%+v", resp.called, resp.gotReq)
	}
	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].text != "answer" {
		t.Fatalf("delivery wrong: %+v", calls)
	}
	if calls[0].route.ChatID != 42 || calls[0].route.ReplyTo != 7 {
		t.Errorf("route not pinned to incoming message: %+v", calls[0].route)
	}
	if sid, ok := d.store.SessionID(42); !ok || sid != "sess-1" {
		t.Errorf("chat->session not bound: %q ok=%v", sid, ok)
	}
}

func TestHandleResumesExistingSession(t *testing.T) {
	resp := &fakeResponder{sid: "sess-new"}
	d := newTestDispatcher(t, resp, &fakeSender{})
	if err := d.store.SetSession(42, "sess-old"); err != nil {
		t.Fatal(err)
	}

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "again"))

	if resp.gotReq.SessionID != "sess-old" {
		t.Errorf("expected resume of sess-old, got %q", resp.gotReq.SessionID)
	}
	if sid, _ := d.store.SessionID(42); sid != "sess-new" {
		t.Errorf("session not updated to sess-new: %q", sid)
	}
}

func TestHandleClearDropsSessionAndSkipsResponder(t *testing.T) {
	resp := &fakeResponder{sid: "x"}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	if err := d.store.SetSession(42, "sess-old"); err != nil {
		t.Fatal(err)
	}

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "/clear"))

	if resp.called {
		t.Errorf("responder should not run on /clear")
	}
	if _, ok := d.store.SessionID(42); ok {
		t.Errorf("session not cleared")
	}
	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].route.ChatID != 42 {
		t.Errorf("clear ack not sent: %+v", calls)
	}
}

func TestIsClearCommand(t *testing.T) {
	for _, s := range []string{"/clear", "/clear@mybot", "  /clear  "} {
		if !isClearCommand(s) {
			t.Errorf("%q should be /clear", s)
		}
	}
	for _, s := range []string{"", "clear", "/clearing", "please /clear"} {
		if isClearCommand(s) {
			t.Errorf("%q should NOT be /clear", s)
		}
	}
}
