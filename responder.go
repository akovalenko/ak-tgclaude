package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RespondRequest is one invocation of the responder: answer Prompt, resuming
// SessionID if set, dropping outbound messages into OutboxDir.
type RespondRequest struct {
	Prompt    string
	SessionID string // resume this session; empty => start a fresh one
	OutboxDir string // AK_TGCLAUDE_OUTBOX for the responder's `send` calls
}

// RespondResult reports the session the responder used, so the dispatcher can
// bind chat→session for continuity.
type RespondResult struct {
	SessionID string
}

// Responder answers one update. The dispatcher depends on this interface so the
// loop is testable without spawning Claude Code.
type Responder interface {
	Respond(ctx context.Context, req RespondRequest) (RespondResult, error)
}

// claudeResponder spawns a headless `claude -p` for each update.
type claudeResponder struct {
	agent string // --agent <name>; empty => the configured default agent
	cwd   string // responder cwd (the materialized scaffold: settings.json + skills)
}

// Respond runs `claude -p [--agent] [--resume] --output-format json`, feeding
// the prompt on stdin and pointing the responder's `send` at OutboxDir. It
// returns the session id parsed from the JSON result.
func (c *claudeResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	cmd := exec.CommandContext(ctx, "claude", buildClaudeArgs(c.agent, req.SessionID)...)
	cmd.Dir = c.cwd
	cmd.Env = append(os.Environ(), outboxEnv+"="+req.OutboxDir)
	cmd.Stdin = strings.NewReader(req.Prompt)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return RespondResult{}, fmt.Errorf("claude -p: %w", err)
	}
	return RespondResult{SessionID: parseSessionID(out.Bytes())}, nil
}

// buildClaudeArgs assembles the `claude -p` argument list. Permission/sandbox
// settings come from the responder cwd's project settings (loaded via
// --setting-sources project once the scaffold materializes), not from here.
func buildClaudeArgs(agent, sessionID string) []string {
	args := []string{"-p", "--output-format", "json"}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return args
}

// stubResponder is a no-model responder for smoke-testing the Telegram I/O path
// (getUpdates -> route -> outbox -> drain -> sendMessage) without spawning
// Claude Code or provisioning a scaffold. It answers every message with a fixed
// line, dropped through the real outbox so the whole delivery path runs.
type stubResponder struct {
	reply string // default "i am here"
}

const defaultStubReply = "i am here"

func (s *stubResponder) Respond(_ context.Context, req RespondRequest) (RespondResult, error) {
	reply := s.reply
	if reply == "" {
		reply = defaultStubReply
	}
	if _, err := (&Descriptor{Kind: KindText, Text: reply}).Drop(req.OutboxDir); err != nil {
		return RespondResult{}, err
	}
	return RespondResult{}, nil
}

// parseSessionID extracts session_id from `claude --output-format json` output.
func parseSessionID(jsonOut []byte) string {
	var r struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(jsonOut, &r)
	return r.SessionID
}
