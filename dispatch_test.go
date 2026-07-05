package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
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
	mcp, err := newMCPServer(sender, "test", false, nil)
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

func TestPersonaFor(t *testing.T) {
	d := &Dispatcher{
		defaultPersona: persona{selectors: []string{"normal"}},
		personas:       map[int64]persona{42: {selectors: []string{"norefuse", "introspect"}}},
	}
	// A configured user resolves to its override selectors (the --debug dump label).
	if got := d.personaFor(42).selectors; len(got) != 2 || got[0] != "norefuse" || got[1] != "introspect" {
		t.Errorf("configured user selectors = %v, want [norefuse introspect]", got)
	}
	// An unknown user falls back to the default — so a non-admin account shows the
	// default stance, not any per-user override.
	if got := d.personaFor(999).selectors; len(got) != 1 || got[0] != "normal" {
		t.Errorf("unknown user selectors = %v, want [normal]", got)
	}
}

func TestHandleDebugDumpsPersona(t *testing.T) {
	// With --debug, a fresh spawn logs the resolved persona: the selector label plus
	// the composed --append-system-prompt text. Models Anton's case — a non-owner
	// account (id 5, no override) resolves to the DEFAULT stance, here norefuse.
	resp := &fakeResponder{sid: "sess-1"}
	d := newTestDispatcher(t, resp, &fakeSender{})
	d.debug = true
	d.defaultPersona = persona{text: "You are a do-what-you're-asked assistant.", selectors: []string{"norefuse"}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	up := Update{UpdateID: 1, Message: &Message{
		MessageID: 100, Text: "hi", Chat: Chat{ID: 42}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)

	out := buf.String()
	if !strings.Contains(out, "persona chat=42 user=5") || !strings.Contains(out, "selectors=[norefuse]") {
		t.Errorf("debug persona line missing/wrong:\n%s", out)
	}
	if !strings.Contains(out, "do-what-you're-asked") {
		t.Errorf("debug dump should include the composed append-system-prompt:\n%s", out)
	}
}

func TestHandleNoDebugNoPersonaDump(t *testing.T) {
	// Without --debug, the persona is not logged.
	d := newTestDispatcher(t, &fakeResponder{sid: "sess-1"}, &fakeSender{})
	d.defaultPersona = persona{text: "You are a do-what-you're-asked assistant.", selectors: []string{"norefuse"}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	d.handleUpdate(context.Background(), textUpdate(1, 42, 100, "hi"))
	if strings.Contains(buf.String(), "persona chat=") {
		t.Errorf("persona should not be logged without --debug:\n%s", buf.String())
	}
}

func TestHandleRecordsUserTurn(t *testing.T) {
	resp := &fakeResponder{} // no replies => only the user turn is recorded
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	root := t.TempDir()
	d.transcripts = NewTranscriptStore(root)

	sent := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local) // noon: no midnight-rollover flake
	up := Update{UpdateID: 1, Message: &Message{
		MessageID: 100, Text: "recall this", Date: sent.Unix(),
		Chat:    Chat{ID: 42},
		From:    &User{ID: 5, Username: "anton", FirstName: "Anton"},
		ReplyTo: &Message{MessageID: 55},
	}}
	d.handleUpdate(context.Background(), up)

	lines := readLines(t, filepath.Join(root, "42", sent.Format("2006-01-02")+".jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 user line, got %d", len(lines))
	}
	var rec TranscriptRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.MsgID != 100 || rec.Role != "user" || rec.ReplyTo != 55 || rec.Text != "recall this" {
		t.Errorf("user record wrong: %+v", rec)
	}
	var meta transcriptMeta
	b, _ := os.ReadFile(filepath.Join(root, "42", "meta.json"))
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Username != "anton" || meta.FirstName != "Anton" || meta.UserCount != 1 {
		t.Errorf("meta wrong: %+v", meta)
	}
}

func TestAttachMeta(t *testing.T) {
	doc := attachMeta(&Message{Document: &Document{}}, &Attachment{Filename: "spec.pdf", MimeType: "application/pdf", Size: 2048})
	if len(doc) != 1 || doc[0].Kind != "document" || doc[0].Name != "spec.pdf" || doc[0].Size != 2048 || doc[0].Mime != "application/pdf" {
		t.Errorf("document attach wrong: %+v", doc)
	}
	photo := attachMeta(&Message{Photo: []PhotoSize{{}}}, &Attachment{Filename: "photo.jpg", MimeType: "image/jpeg", Size: 100})
	if len(photo) != 1 || photo[0].Kind != "photo" {
		t.Errorf("photo attach wrong: %+v", photo)
	}
	if attachMeta(&Message{}, nil) != nil {
		t.Error("nil attachment should yield nil")
	}
}

func TestScopeSelection(t *testing.T) {
	cases := []struct {
		name          string
		root          string
		owner         int64
		ownerReadsAll bool
		fromID        int64
		wantScope     string
	}{
		{"feature off", "", 5, true, 5, ""},
		{"owner reads all -> whole root", "/s/tr", 5, true, 5, "/s/tr"},
		{"owner but reads-all off -> own subdir", "/s/tr", 5, false, 5, "/s/tr/42"},
		{"non-owner -> own subdir", "/s/tr", 5, true, 9, "/s/tr/42"},
		{"no owner configured -> own subdir", "/s/tr", 0, true, 9, "/s/tr/42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &fakeResponder{}
			d := newTestDispatcher(t, resp, &fakeSender{})
			d.transcriptRoot = tc.root
			d.owner = tc.owner
			d.ownerReadsAll = tc.ownerReadsAll

			up := Update{UpdateID: 1, Message: &Message{
				MessageID: 7, Text: "hi", Chat: Chat{ID: 42}, From: &User{ID: tc.fromID},
			}}
			d.handleUpdate(context.Background(), up)

			if resp.gotReq.TranscriptScope != tc.wantScope {
				t.Errorf("scope = %q, want %q", resp.gotReq.TranscriptScope, tc.wantScope)
			}
		})
	}
}

func TestUsageLogAccessSplit(t *testing.T) {
	// The usage-log path reaches the responder on EVERY invocation when the feature is
	// on (empty when off); UsageLogOwner is true only for the configured owner. The
	// responder turns that into an allowRead (owner) vs denyRead (everyone else).
	cases := []struct {
		name      string
		path      string
		owner     int64
		fromID    int64
		wantPath  string
		wantOwner bool
	}{
		{"feature off", "", 5, 9, "", false},
		{"owner", "/v/u.jsonl", 5, 5, "/v/u.jsonl", true},
		{"non-owner", "/v/u.jsonl", 5, 9, "/v/u.jsonl", false},
		{"no owner configured", "/v/u.jsonl", 0, 9, "/v/u.jsonl", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &fakeResponder{}
			d := newTestDispatcher(t, resp, &fakeSender{})
			d.usageLogPath = tc.path
			d.owner = tc.owner

			up := Update{UpdateID: 1, Message: &Message{
				MessageID: 7, Text: "hi", Chat: Chat{ID: 42}, From: &User{ID: tc.fromID},
			}}
			d.handleUpdate(context.Background(), up)

			if resp.gotReq.UsageLogPath != tc.wantPath {
				t.Errorf("UsageLogPath = %q, want %q", resp.gotReq.UsageLogPath, tc.wantPath)
			}
			if resp.gotReq.UsageLogOwner != tc.wantOwner {
				t.Errorf("UsageLogOwner = %v, want %v", resp.gotReq.UsageLogOwner, tc.wantOwner)
			}
		})
	}
}

func TestHandleTranscriptsOffWritesNothing(t *testing.T) {
	resp := &fakeResponder{replies: []string{"answer"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender) // d.transcripts == nil (feature off)
	root := t.TempDir()

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hi"))

	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("feature off should write nothing, found %v", entries)
	}
	if len(sender.snapshot()) != 1 {
		t.Error("delivery should still work with the feature off")
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

// scriptedResponder delivers a different set of send_message replies on each
// successive Respond call (rounds[i] for the i-th call; past the end it delivers
// nothing) and records the prompt it got each time — so a test can simulate a
// responder that sends nothing first, then something on the guard's re-prompt.
type scriptedResponder struct {
	sid     string
	rounds  [][]string
	costs   []float64 // per-call cost (costs[i] for the i-th call; 0 past the end)
	calls   int
	prompts []string
}

func (s *scriptedResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	s.prompts = append(s.prompts, req.Prompt)
	var replies []string
	if s.calls < len(s.rounds) {
		replies = s.rounds[s.calls]
	}
	var cost float64
	if s.calls < len(s.costs) {
		cost = s.costs[s.calls]
	}
	s.calls++
	for _, text := range replies {
		if err := mcpStubSend(ctx, req.MCPURL, req.MCPToken, text); err != nil {
			return RespondResult{}, err
		}
	}
	return RespondResult{SessionID: s.sid, CostUSD: cost}, nil
}

func TestDeliveryGuardRepromptsThenDelivers(t *testing.T) {
	// First turn sends nothing (answer dumped into discarded final text); the guard
	// re-prompts the same session, and the second turn actually delivers.
	resp := &scriptedResponder{sid: "s", rounds: [][]string{nil, {"the real answer"}}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true
	d.undeliveredText = "fallback (should not be used)"

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 2 {
		t.Fatalf("expected original + one re-prompt, got %d calls", resp.calls)
	}
	if resp.prompts[0] != "question" || resp.prompts[1] != redeliverPrompt {
		t.Errorf("re-prompt not the redeliver nudge: %q", resp.prompts[1])
	}
	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].text != "the real answer" {
		t.Fatalf("expected exactly the re-delivered answer, got %+v", calls)
	}
}

func TestDeliveryGuardFallbackWhenStillSilent(t *testing.T) {
	// The responder never sends, even after the re-prompt: the guard sends the
	// undelivered-text fallback so the user is not left with silence.
	resp := &scriptedResponder{sid: "s"} // no rounds => every call delivers nothing
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true
	d.undeliveredText = "sorry, the model could not answer"

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 2 {
		t.Fatalf("expected original + one re-prompt, got %d calls", resp.calls)
	}
	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].text != "sorry, the model could not answer" {
		t.Fatalf("expected the fallback message, got %+v", calls)
	}
	if calls[0].route.ChatID != 42 || calls[0].route.ReplyTo != 7 {
		t.Errorf("fallback not routed to the incoming message: %+v", calls[0].route)
	}
}

func TestDeliveryGuardSilentNoFallbackText(t *testing.T) {
	// Guard on but no undelivered_text: it re-prompts once and then stays quiet
	// (only logs) — no fabricated fallback message.
	resp := &scriptedResponder{sid: "s"}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 2 {
		t.Fatalf("expected original + one re-prompt, got %d calls", resp.calls)
	}
	if calls := sender.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no message without undelivered_text, got %+v", calls)
	}
}

func TestDeliveryGuardOffAllowsSilentTurn(t *testing.T) {
	// With the guard disabled (allow_silent), a no-send turn is left alone: no
	// re-prompt, no fallback.
	resp := &scriptedResponder{sid: "s"}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = false

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 1 {
		t.Fatalf("guard off must not re-prompt, got %d calls", resp.calls)
	}
	if calls := sender.snapshot(); len(calls) != 0 {
		t.Fatalf("guard off must send nothing, got %+v", calls)
	}
}

func TestDeliveryGuardQuietWhenDelivered(t *testing.T) {
	// The common case: the responder delivered on the first turn, so the guard does
	// not fire even though it is on.
	resp := &scriptedResponder{sid: "s", rounds: [][]string{{"answer"}}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true
	d.undeliveredText = "fallback (should not be used)"

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 1 {
		t.Fatalf("guard must not re-prompt after a delivery, got %d calls", resp.calls)
	}
	calls := sender.snapshot()
	if len(calls) != 1 || calls[0].text != "answer" {
		t.Fatalf("expected just the original answer, got %+v", calls)
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

func TestHandleUsageLogWritesRow(t *testing.T) {
	resp := &fakeResponder{sid: "s", cost: 0.0123, replies: []string{"answer"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	var err error
	if d.usage, err = NewUsageLog(path); err != nil {
		t.Fatal(err)
	}

	up := Update{UpdateID: 1, Message: &Message{
		MessageID: 7, Text: "hi", Chat: Chat{ID: 42}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)

	rows := readUsageRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("want exactly one usage row, got %d", len(rows))
	}
	r := rows[0]
	if r.ChatID != 42 || r.UserID != 5 || r.MsgID != 7 {
		t.Errorf("row ids = chat %d user %d msg %d, want 42/5/7", r.ChatID, r.UserID, r.MsgID)
	}
	if r.Cost != 0.0123 {
		t.Errorf("row cost = %v, want 0.0123", r.Cost)
	}
	if r.Elapsed < 0 {
		t.Errorf("elapsed should be >= 0, got %d", r.Elapsed)
	}
	if r.TS.IsZero() {
		t.Errorf("row ts should be set")
	}
}

func TestHandleUsageLogOffWritesNothing(t *testing.T) {
	// No usage_log configured (d.usage nil): a normal round writes no file.
	resp := &fakeResponder{sid: "s", cost: 1, replies: []string{"answer"}}
	d := newTestDispatcher(t, resp, &fakeSender{})
	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "hi")) // must not panic
}

func TestHandleUsageLogSumsRepromptCost(t *testing.T) {
	// First turn sends nothing (cost 0.01), the guard re-prompts and the second
	// delivers (cost 0.02): the row's cost is the whole-round sum, 0.03.
	resp := &scriptedResponder{sid: "s", rounds: [][]string{nil, {"answer"}}, costs: []float64{0.01, 0.02}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	var err error
	if d.usage, err = NewUsageLog(path); err != nil {
		t.Fatal(err)
	}

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	if resp.calls != 2 {
		t.Fatalf("expected a re-prompt (2 calls), got %d", resp.calls)
	}
	rows := readUsageRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("want one row for the whole round, got %d", len(rows))
	}
	// Float sum: compare with a small epsilon rather than ==.
	if got := rows[0].Cost; got < 0.0299 || got > 0.0301 {
		t.Errorf("round cost = %v, want 0.03 (0.01 + re-prompt 0.02)", got)
	}
}

func TestHandleBillCountsRepromptCost(t *testing.T) {
	// The bill and the usage log share one round-cost: a re-prompt's cost is billed
	// too (previously the bill dropped it). 0.01 + 0.02 => "$0.030".
	resp := &scriptedResponder{sid: "s", rounds: [][]string{nil, {"answer"}}, costs: []float64{0.01, 0.02}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.requireDelivery = true
	d.bill = true

	d.handleUpdate(context.Background(), textUpdate(1, 42, 7, "question"))

	calls := sender.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want the re-delivered answer + a bill, got %d: %+v", len(calls), calls)
	}
	if calls[1].text != "$0.030" {
		t.Errorf("bill = %q, want $0.030 (round sum incl. re-prompt)", calls[1].text)
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

func TestPersonaForChat(t *testing.T) {
	d := &Dispatcher{
		groupDefaultPersona: persona{text: "GROUP-DEFAULT", selectors: []string{"norefuse"}},
		personas:            map[int64]persona{-100: {text: "GROUP-100", selectors: []string{"introspect"}}},
	}
	if got := d.personaForChat(-100); got.text != "GROUP-100" {
		t.Errorf("specific group persona = %q, want GROUP-100", got.text)
	}
	if got := d.personaForChat(-999); got.text != "GROUP-DEFAULT" {
		t.Errorf("unknown group persona = %q, want GROUP-DEFAULT", got.text)
	}
}

func TestMentionsBot(t *testing.T) {
	d := &Dispatcher{botUsername: "mybot"}
	for _, tc := range []struct {
		name, text, caption string
		want                bool
	}{
		{"in text", "hey @mybot what is X", "", true},
		{"in caption", "", "look @MyBot at this", true},
		{"case-insensitive", "@MYBOT hi", "", true},
		{"prefix collision", "@mybot2 is someone else", "", false},
		{"trailing punctuation ok", "thanks @mybot, please", "", true},
		{"no mention", "just chatting here", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.mentionsBot(&Message{Text: tc.text, Caption: tc.caption}); got != tc.want {
				t.Errorf("mentionsBot(text=%q,caption=%q) = %v, want %v", tc.text, tc.caption, got, tc.want)
			}
		})
	}
	// An empty botUsername (getMe failed) disables the check.
	off := &Dispatcher{}
	if off.mentionsBot(&Message{Text: "@mybot hi"}) {
		t.Errorf("mentionsBot should be false when botUsername is empty")
	}
}

func TestAddressed(t *testing.T) {
	d := &Dispatcher{botUsername: "mybot"}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"/do", true},
		{"/do@mybot fix it", true},
		{"@mybot help", true},
		{"just a normal line", false},
	} {
		if got := d.addressed(&Message{Text: tc.text}); got != tc.want {
			t.Errorf("addressed(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestHandleGroupUnauthorizedRecordsButSilent(t *testing.T) {
	resp := &fakeResponder{}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	d.authz = newAllowList(nil) // deny everyone
	root := t.TempDir()
	d.transcripts = NewTranscriptStore(root)
	sent := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)
	up := Update{Message: &Message{
		MessageID: 100, Text: "hello room", Date: sent.Unix(),
		Chat: Chat{ID: -100, Type: "supergroup"},
		From: &User{ID: 5, Username: "stranger", FirstName: "S"},
	}}
	d.handleUpdate(context.Background(), up)
	if resp.called {
		t.Errorf("an unauthorized group message must not spawn a responder")
	}
	if len(sender.snapshot()) != 0 {
		t.Errorf("an unauthorized group message must get no reply, got %v", sender.snapshot())
	}
	lines := readLines(t, filepath.Join(root, "-100", sent.Format("2006-01-02")+".jsonl"))
	if len(lines) != 1 {
		t.Fatalf("chatter should still be recorded, got %d lines", len(lines))
	}
	var rec TranscriptRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	// Name is the first_name and Username the @handle, kept apart.
	if rec.User != 5 || rec.Name != "S" || rec.Username != "stranger" || rec.Text != "hello room" {
		t.Errorf("group record wrong (author must be attributed): %+v", rec)
	}
}

func TestHandleGroupAuthorizedNotAddressedRecordsNoSpawn(t *testing.T) {
	resp := &fakeResponder{}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender) // openAccess authorizes everyone
	d.botUsername = "mybot"
	root := t.TempDir()
	d.transcripts = NewTranscriptStore(root)
	sent := time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)
	up := Update{Message: &Message{
		MessageID: 101, Text: "just chatting here", Date: sent.Unix(),
		Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)
	if resp.called {
		t.Errorf("an unaddressed group message must not spawn a responder")
	}
	if len(sender.snapshot()) != 0 {
		t.Errorf("an unaddressed group message must get no reply")
	}
	lines := readLines(t, filepath.Join(root, "-100", sent.Format("2006-01-02")+".jsonl"))
	if len(lines) != 1 {
		t.Fatalf("chatter should be recorded, got %d", len(lines))
	}
}

func TestHandleGroupAddressedSpawns(t *testing.T) {
	resp := &fakeResponder{sid: "s1"}
	d := newTestDispatcher(t, resp, &fakeSender{})
	d.botUsername = "mybot"
	d.transcripts = NewTranscriptStore(t.TempDir())
	up := Update{Message: &Message{
		MessageID: 102, Text: "@mybot what is X",
		Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)
	if !resp.called {
		t.Errorf("an @mention in a group must spawn a responder")
	}
	if resp.gotReq.Prompt != "@mybot what is X" {
		t.Errorf("prompt = %q — the mention must not be stripped", resp.gotReq.Prompt)
	}
}

func TestHandleGroupDoAddresses(t *testing.T) {
	resp := &fakeResponder{sid: "s1"}
	d := newTestDispatcher(t, resp, &fakeSender{})
	// botUsername empty: /do still addresses the bot (no @mention needed).
	up := Update{Message: &Message{
		MessageID: 103, Text: "/do summarize",
		Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)
	if !resp.called {
		t.Errorf("/do in a group must spawn a responder")
	}
}

func TestParseDoTask(t *testing.T) {
	// Inline: the text after /do is the task, authored by the commander.
	inline := parseDoTask(&Message{Text: "/do summarize the log", From: &User{ID: 5}})
	if inline.text != "summarize the log" || inline.delegated || inline.author.ID != 5 {
		t.Errorf("inline = %+v", inline)
	}
	// /do@bot with a payload strips the command token.
	at := parseDoTask(&Message{Text: "/do@mybot fix it", From: &User{ID: 5}})
	if at.text != "fix it" || at.delegated {
		t.Errorf("/do@bot = %+v", at)
	}
	// Reply-form: the replied-to content is the task, authored by that sender.
	reply := parseDoTask(&Message{
		Text:    "/do",
		From:    &User{ID: 5},
		ReplyTo: &Message{MessageID: 77, Text: "how do I restart X?", From: &User{ID: 9, Username: "guest"}},
	})
	if reply.text != "how do I restart X?" || !reply.delegated || reply.author.ID != 9 || reply.src.MessageID != 77 {
		t.Errorf("reply = %+v", reply)
	}
	// Bare /do with no reply yields an empty task.
	if bare := parseDoTask(&Message{Text: "/do", From: &User{ID: 5}}); bare.text != "" {
		t.Errorf("bare = %+v", bare)
	}
}

func TestHandleDoReplyDelegates(t *testing.T) {
	resp := &fakeResponder{sid: "s1", replies: []string{"ok"}}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	up := Update{Message: &Message{
		MessageID: 200, Text: "/do", Chat: Chat{ID: -100, Type: "supergroup"},
		From:    &User{ID: 5},
		ReplyTo: &Message{MessageID: 77, Text: "how do I restart X?", From: &User{ID: 9, Username: "guest"}},
	}}
	d.handleUpdate(context.Background(), up)
	if !resp.called {
		t.Fatal("/do reply must spawn a responder")
	}
	if resp.gotReq.Prompt != "how do I restart X?" {
		t.Errorf("prompt = %q, want the replied-to content", resp.gotReq.Prompt)
	}
	if !resp.gotReq.Delegated || resp.gotReq.DelegatedAuthor == "" {
		t.Errorf("delegation not marked: %+v", resp.gotReq)
	}
	// The bot's reply threads under the ORIGINAL message (77), not the /do.
	calls := sender.snapshot()
	if len(calls) == 0 || calls[len(calls)-1].route.ReplyTo != 77 {
		t.Errorf("reply should thread under the original (77): %+v", calls)
	}
}

func TestHandleDoInline(t *testing.T) {
	resp := &fakeResponder{sid: "s1"}
	d := newTestDispatcher(t, resp, &fakeSender{})
	up := Update{Message: &Message{
		MessageID: 201, Text: "/do summarize", Chat: Chat{ID: 42}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)
	if !resp.called || resp.gotReq.Prompt != "summarize" || resp.gotReq.Delegated {
		t.Errorf("inline /do wrong: called=%v req=%+v", resp.called, resp.gotReq)
	}
}

func TestHandleDoBareUsage(t *testing.T) {
	resp := &fakeResponder{sid: "s1"}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	up := Update{Message: &Message{
		MessageID: 202, Text: "/do", Chat: Chat{ID: 42}, From: &User{ID: 5},
	}}
	d.handleUpdate(context.Background(), up)
	if resp.called {
		t.Errorf("a bare /do with no reply must not spawn")
	}
	calls := sender.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "/do") {
		t.Errorf("bare /do should send a usage hint, got %v", calls)
	}
}

func TestIsEmptyMessage(t *testing.T) {
	if isEmptyMessage(&Message{Text: "hi"}) {
		t.Error("a text message is not empty")
	}
	if isEmptyMessage(&Message{Caption: "cap"}) {
		t.Error("a captioned message is not empty")
	}
	if isEmptyMessage(&Message{Document: &Document{FileID: "D"}}) {
		t.Error("a document message is not empty")
	}
	if isEmptyMessage(&Message{Photo: []PhotoSize{{FileID: "P"}}}) {
		t.Error("a photo message is not empty")
	}
	if !isEmptyMessage(&Message{}) {
		t.Error("a message with no text/caption/file is empty (service message, sticker)")
	}
}

func TestHandleEmptyMessageIgnored(t *testing.T) {
	resp := &fakeResponder{}
	sender := &fakeSender{}
	d := newTestDispatcher(t, resp, sender)
	root := t.TempDir()
	d.transcripts = NewTranscriptStore(root)
	// A Telegram service message: no text, no caption, no attachment (e.g. the bot
	// was added to the group). Must be dropped: no spawn, no reply, no transcript line.
	up := Update{Message: &Message{MessageID: 300, Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5}}}
	d.handleUpdate(context.Background(), up)
	if resp.called {
		t.Error("an empty/service message must not spawn a responder")
	}
	if len(sender.snapshot()) != 0 {
		t.Error("an empty/service message must get no reply")
	}
	if _, err := os.Stat(filepath.Join(root, "-100")); !os.IsNotExist(err) {
		t.Errorf("an empty message must not be recorded (chat dir stat err = %v)", err)
	}
}
