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

	// With an outbox, a --settings overlay granting just that outbox is inserted
	// before --agent/--resume.
	got := buildClaudeArgs("eputs-telegram-guide", "sess-7", "/run/out/outbox-A1")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--settings ") {
		t.Fatalf("expected --settings overlay: %q", joined)
	}
	if !strings.Contains(joined, `Write(/run/out/outbox-A1/**)`) ||
		!strings.Contains(joined, `"allowWrite":["/run/out/outbox-A1"]`) {
		t.Errorf("overlay missing per-invocation grants: %q", joined)
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
	if !strings.Contains(s, `"allow":["Write(/o/x/**)"]`) || !strings.Contains(s, `"allowWrite":["/o/x"]`) {
		t.Errorf("overlay JSON wrong: %s", s)
	}
	// Must NOT touch sandbox.enabled etc. (would clobber the merged base).
	if strings.Contains(s, "enabled") {
		t.Errorf("overlay should only carry allowWrite, got: %s", s)
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

func TestParseSessionID(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","session_id":"abc-123","total_cost_usd":0.01}`)
	if got := parseSessionID(out); got != "abc-123" {
		t.Errorf("session_id = %q, want abc-123", got)
	}
	if got := parseSessionID([]byte("not json")); got != "" {
		t.Errorf("malformed output should yield empty session id, got %q", got)
	}
}
