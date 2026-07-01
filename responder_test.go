package main

import (
	"context"
	"strings"
	"testing"
)

func TestBuildClaudeArgs(t *testing.T) {
	base := "-p --output-format json --setting-sources project --permission-mode dontAsk"

	// No outbox => no --settings overlay.
	if got := strings.Join(buildClaudeArgs("", "", ""), " "); got != base {
		t.Errorf("bare args = %q", got)
	}

	// With an outbox, a --settings overlay scoping sandbox access to that outbox
	// is inserted before --agent/--resume.
	got := buildClaudeArgs("eputs-telegram-guide", "sess-7", "/run/out/outbox-A1")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--settings ") {
		t.Fatalf("expected --settings overlay: %q", joined)
	}
	if !strings.Contains(joined, `"allowWrite":["/run/out/outbox-A1"]`) ||
		!strings.Contains(joined, `"allowRead":["/run/out/outbox-A1"]`) {
		t.Errorf("overlay missing per-invocation sandbox grants: %q", joined)
	}
	if !strings.HasSuffix(joined, "--agent eputs-telegram-guide --resume sess-7") {
		t.Errorf("agent/resume should come after --settings: %q", joined)
	}
}

func TestBuildInvocationSettings(t *testing.T) {
	if buildInvocationSettings("") != "" {
		t.Errorf("empty outbox => empty overlay")
	}
	s := buildInvocationSettings("/o/x")
	if !strings.Contains(s, `"allowWrite":["/o/x"]`) || !strings.Contains(s, `"allowRead":["/o/x"]`) {
		t.Errorf("overlay JSON wrong: %s", s)
	}
	// The Write TOOL grant is the hook's job now — the overlay is sandbox-only.
	if strings.Contains(s, "permissions") || strings.Contains(s, "Write(") {
		t.Errorf("overlay should carry no permissions/Write, got: %s", s)
	}
	// Must NOT touch sandbox.enabled etc. (would clobber the merged base).
	if strings.Contains(s, "enabled") {
		t.Errorf("overlay should only carry filesystem allow*, got: %s", s)
	}
}

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("/home/bot/code", "/run/out/outbox-A1", "how does foo work?")
	if !strings.Contains(p, "Project directory (read-only): /home/bot/code") {
		t.Errorf("missing literal project path: %q", p)
	}
	if !strings.Contains(p, "Outbox directory (write your reply body files here): /run/out/outbox-A1") {
		t.Errorf("missing literal outbox path: %q", p)
	}
	if !strings.Contains(p, "not shell-expanded") {
		t.Errorf("missing the tool-vs-shell caveat: %q", p)
	}
	// The message comes last, after the preamble.
	if !strings.HasSuffix(p, "how does foo work?") {
		t.Errorf("message should be appended last: %q", p)
	}
}

func TestStubResponderRepliesFixed(t *testing.T) {
	dir := t.TempDir()
	res, err := (&stubResponder{}).Respond(context.Background(), RespondRequest{OutboxDir: dir, Prompt: "any question"})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.SessionID != "" {
		t.Errorf("stub should not bind a session, got %q", res.SessionID)
	}
	d, _ := readOnlyDescriptor(t, dir) // helper from outbox_test.go
	if d.Kind != KindText || d.Text != "i am here" {
		t.Errorf("stub descriptor wrong: %+v", d)
	}
}

func TestParseResult(t *testing.T) {
	out := []byte(`{"type":"result","session_id":"abc-123","result":"Sent the answer.\nanswered"}`)
	sid, outcome, final := parseResult(out)
	if sid != "abc-123" {
		t.Errorf("session_id = %q, want abc-123", sid)
	}
	if outcome != "answered" {
		t.Errorf("outcome = %q, want answered", outcome)
	}
	if final != "Sent the answer.\nanswered" {
		t.Errorf("final text = %q", final)
	}
	if s, o, f := parseResult([]byte("not json")); s != "" || o != "" || f != "" {
		t.Errorf("malformed => %q/%q/%q, want empty", s, o, f)
	}
}

func TestParseOutcome(t *testing.T) {
	// Exact last line wins.
	if got := parseOutcome("did stuff\nrefused"); got != "refused" {
		t.Errorf("last-line exact => %q", got)
	}
	// Trailing punctuation / markdown tolerated.
	if got := parseOutcome("ok\n**problematic**"); got != "problematic" {
		t.Errorf("punctuation-wrapped => %q", got)
	}
	// None present.
	if got := parseOutcome("here is your answer"); got != "" {
		t.Errorf("no outcome => %q, want empty", got)
	}
	// Fallback: last occurrence anywhere when the last line isn't exact.
	if got := parseOutcome("I answered it fully. Everything is fine now."); got != "answered" {
		t.Errorf("fallback scan => %q, want answered", got)
	}
}
