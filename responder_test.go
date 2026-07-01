package main

import (
	"context"
	"strings"
	"testing"
)

func TestBuildClaudeArgs(t *testing.T) {
	base := "-p --output-format json --setting-sources project --permission-mode dontAsk"
	if got := strings.Join(buildClaudeArgs("", ""), " "); got != base {
		t.Errorf("bare args = %q", got)
	}
	got := strings.Join(buildClaudeArgs("eputs-telegram-guide", "sess-7"), " ")
	want := base + " --agent eputs-telegram-guide --resume sess-7"
	if got != want {
		t.Errorf("full args = %q, want %q", got, want)
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
