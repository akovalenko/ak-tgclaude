package main

import (
	"strings"
	"testing"
)

func TestBuildClaudeArgs(t *testing.T) {
	got := strings.Join(buildClaudeArgs("", ""), " ")
	if got != "-p --output-format json" {
		t.Errorf("bare args = %q", got)
	}
	got = strings.Join(buildClaudeArgs("eputs-telegram-guide", "sess-7"), " ")
	want := "-p --output-format json --agent eputs-telegram-guide --resume sess-7"
	if got != want {
		t.Errorf("full args = %q, want %q", got, want)
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
