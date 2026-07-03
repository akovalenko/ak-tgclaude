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

// fakeResponder records the request it got and, simulating the agent's send_*
// tool calls, delivers each reply through the real MCP transport (an actual
// tools/call to the dispatcher's server, authorized by the invocation token).
type fakeResponder struct {
	sid     string
	cost    float64
	replies []string // text messages to emit via send_message
	gotReq  RespondRequest
	called  bool
}

func (f *fakeResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	f.called = true
	f.gotReq = req
	for _, text := range f.replies {
		if err := mcpStubSend(ctx, req.MCPURL, req.MCPToken, text); err != nil {
			return RespondResult{}, err
		}
	}
	return RespondResult{SessionID: f.sid, CostUSD: f.cost}, nil
}

func newTestDispatcher(t *testing.T, resp Responder, sender Sender) *Dispatcher {
	t.Helper()
	store, err := LoadSessionStore(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := newMCPServer(sender, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mcp.Close() })
	return &Dispatcher{
		sender:      sender,
		mcp:         mcp,
		store:       store,
		resp:        resp,
		authz:       openAccess{}, // tests that don't exercise access allow everyone
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
	resp := &fakeResponder{sid: "sess-1", replies: []string{"answer"}}
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

func TestBillLine(t *testing.T) {
	cases := []struct {
		cost float64
		want string
		ok   bool
	}{
		{0, "", false},
		{-1, "", false},
		{0.0001, "", false}, // rounds to $0.000 → suppressed
		{0.0123, "$0.012", true},
		{12.3, "$12.300", true},
	}
	for _, tc := range cases {
		got, ok := billLine(tc.cost)
		if got != tc.want || ok != tc.ok {
			t.Errorf("billLine(%v) = %q,%v; want %q,%v", tc.cost, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHandleBillSendsCostAfterAnswer(t *testing.T) {
	resp := &fakeResponder{sid: "s", cost: 0.0123, replies: []string{"answer"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.bill = true

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hi"))

	calls := sender.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want answer + bill, got %d calls: %+v", len(calls), calls)
	}
	if calls[0].text != "answer" {
		t.Errorf("first message should be the answer: %q", calls[0].text)
	}
	if calls[1].text != "$0.012" {
		t.Errorf("bill line = %q, want $0.012", calls[1].text)
	}
	if calls[1].route.ChatID != 42 || calls[1].route.ReplyTo != 7 {
		t.Errorf("bill not routed to the incoming message: %+v", calls[1].route)
	}
}

func TestHandleBillSilentWhenZeroOrDisabled(t *testing.T) {
	// bill enabled but cost is zero => no bill message (just the answer).
	resp := &fakeResponder{sid: "s", cost: 0, replies: []string{"answer"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.bill = true
	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hi"))
	if calls := sender.snapshot(); len(calls) != 1 {
		t.Fatalf("zero cost should send no bill: %+v", calls)
	}

	// bill disabled but cost present => still no bill message.
	resp2 := &fakeResponder{sid: "s", cost: 5, replies: []string{"answer"}}
	sender2 := &fakeSender{}
	d2 := newTestDispatcher(t, resp2, sender2)
	d2.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hi"))
	if calls := sender2.snapshot(); len(calls) != 1 {
		t.Fatalf("bill disabled should send no bill: %+v", calls)
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

func TestHandleDeniesUnauthorized(t *testing.T) {
	// /start and /help get a no-access reply carrying the id; anything else from a
	// denied user gets no reply and never reaches the responder.
	cases := []struct {
		text      string
		wantReply bool
	}{
		{"/start", true},
		{"/help", true},
		{"tell me about X", false},
		{"/clear", false},
	}
	for _, tc := range cases {
		resp := &fakeResponder{}
		sender := &fakeSender{}
		d := newTestDispatcher(t, resp, sender)
		d.authz = newAllowList(nil) // deny everyone
		d.helpText = "HELP"

		u := Update{UpdateID: 1, Message: &Message{
			MessageID: 7, Text: tc.text, Chat: Chat{ID: 42}, From: &User{ID: 999},
		}}
		d.handleUpdate(context.Background(), u)

		if resp.called {
			t.Errorf("%q: responder must not run for a denied user", tc.text)
		}
		calls := sender.snapshot()
		if tc.wantReply {
			if len(calls) != 1 || !strings.Contains(calls[0].text, "999") {
				t.Errorf("%q: want no-access reply mentioning id 999, got %+v", tc.text, calls)
			}
		} else if len(calls) != 0 {
			t.Errorf("%q: denied non-command should get no reply, got %+v", tc.text, calls)
		}
	}
}

func TestHandleAllowedUserPasses(t *testing.T) {
	resp := &fakeResponder{replies: []string{"answer"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.authz = newAllowList([]int64{999})

	u := Update{UpdateID: 1, Message: &Message{
		MessageID: 7, Text: "hello", Chat: Chat{ID: 42}, From: &User{ID: 999},
	}}
	d.handleUpdate(context.Background(), u)

	if !resp.called {
		t.Error("whitelisted user should reach the responder")
	}
	if calls := sender.snapshot(); len(calls) != 1 || calls[0].text != "answer" {
		t.Errorf("whitelisted user's answer not delivered: %+v", calls)
	}
}

func TestHandleHelpHTMLMode(t *testing.T) {
	sender := &fakeSender{}
	d := newTestDispatcher(t, &fakeResponder{}, sender)
	d.helpText = "<b>hi</b>"
	d.helpParseMode = "HTML"

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "/help"))

	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].text != "<b>hi</b>" || calls[0].mode != "HTML" {
		t.Fatalf("help should be sent as HTML: %+v", calls)
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
