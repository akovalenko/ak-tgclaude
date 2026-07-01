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

// RespondResult reports the session the responder used (so the dispatcher can
// bind chat→session), the outcome word the responder ended with, and the raw
// final text (logged when the outcome is unrecognized).
type RespondResult struct {
	SessionID string
	Outcome   string // "answered"|"problematic"|"refused"|"" (from the final output)
	FinalText string // the responder's final result text (for diagnostics)
}

// Responder answers one update. The dispatcher depends on this interface so the
// loop is testable without spawning Claude Code.
type Responder interface {
	Respond(ctx context.Context, req RespondRequest) (RespondResult, error)
}

// projectEnv tells the responder where the project it consults on lives, so its
// agent can explore it (read-only) by absolute path.
const projectEnv = "AK_TGCLAUDE_PROJECT"

// claudeResponder spawns a headless `claude -p` for each update.
type claudeResponder struct {
	agent    string // --agent <name>; empty => the configured default agent
	cwd      string // responder cwd (the materialized scaffold: settings.json + skills)
	project  string // the project the agent answers about ($AK_TGCLAUDE_PROJECT)
	cacheDir string // isolated Go cache root, injected into the process env
}

// Respond runs `claude -p [--agent] [--resume] --output-format json`, feeding
// the prompt on stdin and pointing the responder's `send` at OutboxDir. It
// returns the session id parsed from the JSON result.
func (c *claudeResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	cmd := exec.CommandContext(ctx, "claude", buildClaudeArgs(c.agent, req.SessionID, req.OutboxDir)...)
	cmd.Dir = c.cwd
	cmd.Env = append(os.Environ(), outboxEnv+"="+req.OutboxDir, projectEnv+"="+c.project)
	if c.cacheDir != "" {
		// Inject the isolated Go cache so the sandboxed `go` inherits it (the
		// settings-file env block does not reach tools under --setting-sources).
		for k, v := range goCacheEnv(c.cacheDir) {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = strings.NewReader(buildPrompt(c.project, req.OutboxDir, req.Prompt))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return RespondResult{}, fmt.Errorf("claude -p: %w", err)
	}
	sid, outcome, final := parseResult(out.Bytes())
	return RespondResult{SessionID: sid, Outcome: outcome, FinalText: final}, nil
}

// buildPrompt prepends a small preamble giving the responder the LITERAL
// project and outbox paths, then the incoming (untrusted) message. Literal paths
// matter because the Write/Read tools do not expand env vars in their arguments
// (only the shell does), so the agent must not rely on $AK_TGCLAUDE_* there.
func buildPrompt(project, outbox, message string) string {
	var b strings.Builder
	b.WriteString("Project directory (read-only): ")
	b.WriteString(project)
	b.WriteString("\nOutbox directory (write your reply body files here): ")
	b.WriteString(outbox)
	b.WriteString("\nThese are literal paths — pass them verbatim to the Write/Read tools " +
		"(tool arguments are not shell-expanded); in shell commands the same paths are in " +
		"$AK_TGCLAUDE_PROJECT / $AK_TGCLAUDE_OUTBOX.\n\n")
	b.WriteString("Incoming Telegram message to answer:\n")
	b.WriteString(message)
	return b.String()
}

// buildClaudeArgs assembles the `claude -p` argument list. It loads only the
// responder cwd's project settings (--setting-sources project) so the generated
// scaffold governs sandbox/permissions/hooks, runs headless deny-by-default
// (--permission-mode dontAsk) so an unmatched tool is denied rather than hung
// on, and overlays a per-invocation --settings that grants write to just this
// invocation's outbox (merged on top of the static settings).
func buildClaudeArgs(agent, sessionID, outbox string) []string {
	args := []string{
		"-p", "--output-format", "json",
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
	}
	if s := buildInvocationSettings(outbox); s != "" {
		args = append(args, "--settings", s)
	}
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
	return RespondResult{Outcome: "answered"}, nil
}

// parseResult extracts the session id, the outcome word, and the raw final text
// from `claude --output-format json` output.
func parseResult(jsonOut []byte) (sessionID, outcome, finalText string) {
	var r struct {
		SessionID string `json:"session_id"`
		Result    string `json:"result"`
	}
	_ = json.Unmarshal(jsonOut, &r)
	return r.SessionID, parseOutcome(r.Result), r.Result
}

// knownOutcomes are the status words the responder ends its output with. The
// set is intentionally small and may grow.
var knownOutcomes = []string{"answered", "problematic", "refused"}

// parseOutcome returns the responder's outcome word: the last output line when
// it is exactly one of knownOutcomes, else the last such word appearing
// anywhere, else "".
func parseOutcome(result string) string {
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) > 0 {
		last := strings.ToLower(strings.Trim(strings.TrimSpace(lines[len(lines)-1]), ".!?\"'`*"))
		for _, w := range knownOutcomes {
			if last == w {
				return w
			}
		}
	}
	low := strings.ToLower(result)
	best, bestAt := "", -1
	for _, w := range knownOutcomes {
		if i := strings.LastIndex(low, w); i > bestAt {
			bestAt, best = i, w
		}
	}
	return best
}
