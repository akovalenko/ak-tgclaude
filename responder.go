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
// SessionID if set. The responder emits by calling the dispatcher's MCP send_*
// tools (reachable at MCPURL, authorized by the route-pinned MCPToken); DocDir is
// its writable area for document attachments and scratch files.
type RespondRequest struct {
	Prompt    string
	SessionID string // resume this session; empty => start a fresh one
	DocDir    string // AK_TGCLAUDE_OUTBOX: writable dir for attachments/scratch
	MCPURL    string // dispatcher's MCP endpoint
	MCPToken  string // this invocation's capability token (the server pins the route to it)
}

// RespondResult reports the session the responder used (so the dispatcher can
// bind chat→session), the outcome word the responder ended with, and the raw
// final text (logged when the outcome is unrecognized).
type RespondResult struct {
	SessionID string
	Outcome   string  // "answered"|"problematic"|"refused"|"" (from the final output)
	FinalText string  // the responder's final result text (for diagnostics)
	CostUSD   float64 // total_cost_usd for the run (0 if absent); surfaced by --bill
}

// Responder answers one update. The dispatcher depends on this interface so the
// loop is testable without spawning Claude Code.
type Responder interface {
	Respond(ctx context.Context, req RespondRequest) (RespondResult, error)
}

// projectEnv tells the responder where the project it consults on lives, so its
// agent can explore it (read-only) by absolute path.
const projectEnv = "AK_TGCLAUDE_PROJECT"

// outboxEnv names the responder's writable directory (attachments + scratch). The
// dispatcher sets it per invocation; the PreToolUse hook scopes the Write tool to
// it. The name is historical (it was the descriptor-drop outbox before the MCP
// transport); it now holds only files the responder authors, e.g. a document to
// attach via the send_document tool.
const outboxEnv = "AK_TGCLAUDE_OUTBOX"

// claudeResponder spawns a headless `claude -p` for each update.
type claudeResponder struct {
	agent    string // --agent <name>; empty => the configured default agent
	cwd      string // responder cwd (the materialized scaffold: settings.json + skills)
	project  string // the project the agent answers about ($AK_TGCLAUDE_PROJECT)
	cacheDir string // isolated Go cache root, injected into the process env
}

// Respond runs `claude -p [--agent] [--resume] --output-format json`, feeding
// the prompt on stdin and wiring the responder to the dispatcher's MCP server
// (route-pinned by MCPToken). It returns the session id parsed from the JSON
// result.
func (c *claudeResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	cmd := exec.CommandContext(ctx, "claude", buildClaudeArgs(c.agent, req.SessionID, req.DocDir, req.MCPURL, req.MCPToken)...)
	cmd.Dir = c.cwd
	cmd.Env = append(os.Environ(), outboxEnv+"="+req.DocDir, projectEnv+"="+c.project)
	if c.cacheDir != "" {
		// Inject the isolated Go cache so the sandboxed `go` inherits it (the
		// settings-file env block does not reach tools under --setting-sources).
		for k, v := range goCacheEnv(c.cacheDir) {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = strings.NewReader(buildPrompt(c.project, req.DocDir, req.Prompt))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return RespondResult{}, fmt.Errorf("claude -p: %w", err)
	}
	sid, outcome, final, cost := parseResult(out.Bytes())
	return RespondResult{SessionID: sid, Outcome: outcome, FinalText: final, CostUSD: cost}, nil
}

// buildPrompt prepends a small preamble giving the responder the LITERAL
// project and outbox paths, then the incoming (untrusted) message. Literal paths
// matter because the Write/Read tools do not expand env vars in their arguments
// (only the shell does), so the agent must not rely on $AK_TGCLAUDE_* there. The
// outbox is where the responder writes a document before attaching it with the
// send_document tool (plain/code replies go straight through the send tools, no
// file needed).
func buildPrompt(project, outbox, message string) string {
	var b strings.Builder
	b.WriteString("Project directory (read-only): ")
	b.WriteString(project)
	b.WriteString("\nOutbox directory (write attachment/scratch files here): ")
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
// on, wires the dispatcher's MCP server as the ONLY MCP source
// (--strict-mcp-config, the token in the config header) and permits its send
// tools (--allowedTools; their availability comes from the agent's tools:
// frontmatter), and overlays a per-invocation --settings that grants write to
// just this invocation's outbox (merged on top of the static settings).
func buildClaudeArgs(agent, sessionID, docDir, mcpURL, mcpToken string) []string {
	args := []string{
		"-p", "--output-format", "json",
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
	}
	if mcpURL != "" && mcpToken != "" {
		args = append(args,
			"--mcp-config", buildMCPConfig(mcpURL, mcpToken),
			"--strict-mcp-config",
			"--allowedTools", strings.Join(mcpTools, ","),
		)
	}
	if s := buildInvocationSettings(docDir); s != "" {
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

func (s *stubResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	reply := s.reply
	if reply == "" {
		reply = defaultStubReply
	}
	// Deliver through the real MCP transport (an actual tools/call to the
	// dispatcher's server) so --responder stub smoke-tests the full path:
	// getUpdates -> route -> MCP send_message -> sendMessage with reply threading.
	if err := mcpStubSend(ctx, req.MCPURL, req.MCPToken, reply); err != nil {
		return RespondResult{}, err
	}
	return RespondResult{Outcome: "answered"}, nil
}

// parseResult extracts the session id, the outcome word, the raw final text, and
// the run's dollar cost from `claude --output-format json` output.
func parseResult(jsonOut []byte) (sessionID, outcome, finalText string, costUSD float64) {
	var r struct {
		SessionID    string  `json:"session_id"`
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	_ = json.Unmarshal(jsonOut, &r)
	return r.SessionID, parseOutcome(r.Result), r.Result, r.TotalCostUSD
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
