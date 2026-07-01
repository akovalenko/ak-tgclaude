package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// typingProbe blocks in Respond until the sender has recorded a "typing" chat
// action, proving the dispatcher shows typing for the responder's lifetime.
type typingProbe struct{ sender *fakeSender }

func (p *typingProbe) Respond(ctx context.Context, _ RespondRequest) (RespondResult, error) {
	for i := 0; i < 1000; i++ {
		if p.sender.actionCount() > 0 {
			return RespondResult{Outcome: "answered"}, nil
		}
		select {
		case <-ctx.Done():
			return RespondResult{}, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	return RespondResult{}, errors.New("no typing action observed")
}

func TestHandleShowsTypingDuringResponder(t *testing.T) {
	sender := &fakeSender{}
	d := newTestDispatcher(t, &typingProbe{sender: sender}, sender)

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hello"))

	if sender.actionCount() == 0 {
		t.Fatal("expected a typing chat action during the responder's lifetime")
	}
}

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
		outboxRoot:  t.TempDir(),
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

func TestOutcomeField(t *testing.T) {
	if got := outcomeField(RespondResult{Outcome: "answered"}); got != "answered" {
		t.Errorf("known outcome => %q", got)
	}
	// Unrecognized: "?" + a quoted snippet of the final text.
	long := strings.Repeat("x", 250)
	got := outcomeField(RespondResult{FinalText: long})
	if !strings.HasPrefix(got, `? result="`) || !strings.Contains(got, "…") {
		t.Errorf("unknown outcome should include a quoted, truncated snippet: %q", got)
	}
	if len([]rune(got)) > 130 { // 100-rune snippet + quoting/ellipsis, not the full 250
		t.Errorf("snippet not truncated: %q", got)
	}
	// Unknown with no final text => bare "?".
	if got := outcomeField(RespondResult{}); got != "?" {
		t.Errorf("empty => %q, want ?", got)
	}
}

func TestHandleHelpAndStart(t *testing.T) {
	for _, cmd := range []string{"/help", "/start", "/help@mybot", "/start deep-link"} {
		resp := &fakeResponder{}
		sender := &fakeSender{}
		d := newTestDispatcher(t, resp, sender)
		d.helpText = "HELP BLURB"

		d.handleUpdate(context.Background(), textUpdate(1, 42, 7, cmd))

		if resp.called {
			t.Errorf("%s: responder should not run", cmd)
		}
		calls := sender.snapshot()
		if len(calls) != 1 || calls[0].text != "HELP BLURB" {
			t.Fatalf("%s: help not delivered: %+v", cmd, calls)
		}
		if calls[0].route.ChatID != 42 || calls[0].route.ReplyTo != 7 {
			t.Errorf("%s: help not routed to the incoming message: %+v", cmd, calls[0].route)
		}
		if _, ok := d.store.SessionID(42); ok {
			t.Errorf("%s: help must not bind a session", cmd)
		}
	}
}

func TestIsSlashCommand(t *testing.T) {
	if !isSlashCommand("/start deep-link-payload", "start") {
		t.Error("/start with a payload should match")
	}
	if !isSlashCommand("/help@bot", "help") {
		t.Error("/help@bot should match")
	}
	if isSlashCommand("/helpme", "help") {
		t.Error("/helpme should not match help")
	}
	if isSlashCommand("please /help", "help") {
		t.Error("non-leading /help should not match")
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
