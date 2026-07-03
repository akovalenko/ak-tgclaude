package main

import (
	"context"
	"strings"
	"testing"
)

func TestBuildClaudeArgs(t *testing.T) {
	base := "-p --output-format json --setting-sources project --permission-mode dontAsk"

	// No docDir and no MCP endpoint => bare args (no overlay, no MCP wiring).
	if got := strings.Join(buildClaudeArgs("", "", "", "", ""), " "); got != base {
		t.Errorf("bare args = %q", got)
	}

	got := buildClaudeArgs("eputs-telegram-guide", "sess-7", "/run/out/outbox-A1", "http://127.0.0.1:9/mcp", "tok9")
	joined := strings.Join(got, " ")
	// MCP wiring: the config (url + Authorization token), strict-only, and the
	// send tools permitted under dontAsk.
	if !strings.Contains(joined, "--mcp-config ") || !strings.Contains(joined, "--strict-mcp-config") {
		t.Errorf("expected MCP config args: %q", joined)
	}
	if !strings.Contains(joined, `"url":"http://127.0.0.1:9/mcp"`) || !strings.Contains(joined, "Bearer tok9") {
		t.Errorf("MCP config should carry url + token: %q", joined)
	}
	if !strings.Contains(joined, "--allowedTools mcp__tg__send_message,mcp__tg__send_code,mcp__tg__send_document") {
		t.Errorf("expected --allowedTools with the send tools: %q", joined)
	}
	// A --settings overlay scopes sandbox access to that outbox, before --agent/--resume.
	if !strings.Contains(joined, `"allowWrite":["/run/out/outbox-A1"]`) ||
		!strings.Contains(joined, `"allowRead":["/run/out/outbox-A1"]`) {
		t.Errorf("overlay missing per-invocation sandbox grants: %q", joined)
	}
	if !strings.HasSuffix(joined, "--agent eputs-telegram-guide --resume sess-7") {
		t.Errorf("agent/resume should come last: %q", joined)
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
	if !strings.Contains(p, "Outbox directory (write attachment/scratch files here): /run/out/outbox-A1") {
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
	f := &fakeSender{}
	m := newTestMCP(t, f) // helper from mcp_test.go
	tok, err := m.Register(Route{ChatID: 5, ReplyTo: 2}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&stubResponder{}).Respond(context.Background(), RespondRequest{
		Prompt: "any question", MCPURL: m.URL(), MCPToken: tok,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.SessionID != "" {
		t.Errorf("stub should not bind a session, got %q", res.SessionID)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].text != "i am here" {
		t.Fatalf("stub reply wrong: %+v", calls)
	}
	if calls[0].route.ChatID != 5 || calls[0].route.ReplyTo != 2 {
		t.Errorf("stub reply not routed via the token: %+v", calls[0].route)
	}
}

func TestParseResult(t *testing.T) {
	out := []byte(`{"type":"result","session_id":"abc-123","result":"Sent the answer.\nanswered","total_cost_usd":0.0123}`)
	sid, outcome, final, cost := parseResult(out)
	if sid != "abc-123" {
		t.Errorf("session_id = %q, want abc-123", sid)
	}
	if outcome != "answered" {
		t.Errorf("outcome = %q, want answered", outcome)
	}
	if final != "Sent the answer.\nanswered" {
		t.Errorf("final text = %q", final)
	}
	if cost != 0.0123 {
		t.Errorf("total_cost_usd = %v, want 0.0123", cost)
	}
	if s, o, f, c := parseResult([]byte("not json")); s != "" || o != "" || f != "" || c != 0 {
		t.Errorf("malformed => %q/%q/%q/%v, want empty", s, o, f, c)
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
