package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RespondRequest is one invocation of the responder: answer Prompt, resuming
// SessionID if set. The responder emits by calling the dispatcher's MCP send_*
// tools (reachable at MCPURL, authorized by the route-pinned MCPToken); DocDir is
// its writable area for document attachments and scratch files.
type RespondRequest struct {
	Prompt     string
	SentAt     time.Time   // Telegram send time of the incoming message; zero => omit from the prompt
	Attachment *Attachment // an incoming file saved in the outbox (nil => none)
	SessionID  string      // resume this session; empty => start a fresh one
	DocDir     string      // AK_TGCLAUDE_OUTBOX: writable dir for attachments/scratch
	MCPURL     string      // dispatcher's MCP endpoint (inline --mcp-config for claude; direct call for the stub)
	MCPToken   string      // this invocation's capability token (the server pins the route to it)
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
	agent      string   // --agent <name>; empty => the configured default agent
	cwd        string   // responder cwd (the materialized scaffold: settings.json + skills)
	project    string   // the project the agent answers about ($AK_TGCLAUDE_PROJECT)
	cacheDir   string   // isolated Go cache root, injected into the process env
	debug      bool     // pass --debug to claude -p (diagnostics to stderr)
	claudeArgs []string // operator passthrough appended to claude -p (validated at config load)
	extraTools []string // EXTRA tools granted (config `tools`/--tool): added to --allowedTools
}

// Respond runs `claude -p [--agent] [--resume] --output-format json`, feeding
// the prompt on stdin and wiring the responder to the dispatcher's MCP server
// (route-pinned by MCPToken). It returns the session id parsed from the JSON
// result.
func (c *claudeResponder) Respond(ctx context.Context, req RespondRequest) (RespondResult, error) {
	cmd := exec.CommandContext(ctx, "claude", buildClaudeArgs(c.agent, req.SessionID, req.DocDir, req.MCPURL, req.MCPToken, c.debug, c.extraTools, c.claudeArgs)...)
	cmd.Dir = c.cwd
	cmd.Env = c.env(req.DocDir)
	cmd.Stdin = strings.NewReader(buildPrompt(c.project, req.DocDir, req.Prompt, req.SentAt, req.Attachment))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return RespondResult{}, fmt.Errorf("claude -p: %w", err)
	}
	sid, outcome, final, cost := parseResult(out.Bytes())
	return RespondResult{SessionID: sid, Outcome: outcome, FinalText: final, CostUSD: cost}, nil
}

// env assembles the responder process environment: the inherited env, the
// outbox/project vars, the isolated Go cache, and NO_PROXY/no_proxy forced to
// include loopback.
//
// The proxy part is load-bearing for the MCP transport: the responder's MCP
// client dials the dispatcher's server at http://127.0.0.1:<port>. If the host
// has an HTTP(S)_PROXY set and NO_PROXY does not exempt loopback, that request is
// sent to the upstream proxy — which cannot reach the dispatcher's loopback — so
// the server is never dialed and no tools appear. Forcing loopback into NO_PROXY
// makes the MCP request go direct while everything else (the Anthropic API) still
// honors the proxy. Existing NO_PROXY entries are preserved.
func (c *claudeResponder) env(docDir string) []string {
	noProxy := mergeNoProxy(os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	var out []string
	for _, kv := range os.Environ() {
		// Drop any inherited NO_PROXY/no_proxy; re-added below with loopback merged
		// in (a duplicate key is resolved inconsistently across getenv impls).
		if strings.HasPrefix(kv, "NO_PROXY=") || strings.HasPrefix(kv, "no_proxy=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		outboxEnv+"="+docDir,
		projectEnv+"="+c.project,
		"NO_PROXY="+noProxy,
		"no_proxy="+noProxy,
		// Point temp at the per-invocation outbox: the sandbox derives the sandboxed
		// $TMPDIR as $TMPDIR/claude-<uid>, so temp lands under the outbox and the
		// dispatcher's RemoveAll cleans it (instead of accumulating in /tmp/claude-<uid>).
		"TMPDIR="+docDir,
	)
	if c.cacheDir != "" {
		// The isolated Go cache, so the sandboxed `go` inherits it (a settings-file
		// env block does not reach tools under --setting-sources).
		for k, v := range goCacheEnv(c.cacheDir) {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// mergeNoProxy returns a NO_PROXY value that always includes the loopback hosts
// (so a configured HTTP proxy is bypassed for the dispatcher's localhost MCP
// server), merged with — and de-duplicating — any existing entries.
func mergeNoProxy(existing ...string) string {
	seen := map[string]bool{}
	var out []string
	add := func(list string) {
		for _, h := range strings.Split(list, ",") {
			if h = strings.TrimSpace(h); h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	for _, e := range existing {
		add(e)
	}
	add("127.0.0.1,localhost,::1")
	return strings.Join(out, ",")
}

// buildPrompt prepends a small preamble giving the responder the LITERAL
// project and outbox paths, then the incoming (untrusted) message. Literal paths
// matter because the Write/Read tools do not expand env vars in their arguments
// (only the shell does), so the agent must not rely on $AK_TGCLAUDE_* there. The
// outbox is where the responder writes a document before attaching it with the
// send_document tool (plain/code replies go straight through the send tools, no
// file needed).
//
// sentAt, when non-zero, stamps the message with its Telegram send time so the
// model can reason about elapsed time across a resumed conversation ("we spoke
// about this yesterday"); each turn's prompt carries its own stamp, so the
// accumulated session reads as a dated transcript. It is rendered in the host's
// local time zone with the zone abbreviation, so it is unambiguous.
//
// attach, when non-nil, is an incoming file the dispatcher already saved into
// the outbox; its path and description are announced so the model can read or
// Edit it in place (its content is untrusted, like the message text).
func buildPrompt(project, outbox, message string, sentAt time.Time, attach *Attachment) string {
	var b strings.Builder
	b.WriteString("Project directory (read-only): ")
	b.WriteString(project)
	b.WriteString("\nOutbox directory (write attachment/scratch files here): ")
	b.WriteString(outbox)
	b.WriteString("\nThe outbox PERSISTS across replies in this conversation — build or clone into " +
		"it once and reuse that next turn instead of redoing the work; only $TMPDIR (scratch) is " +
		"cleared after each reply.")
	b.WriteString("\nThese are literal paths — pass them verbatim to the Write/Read tools " +
		"(tool arguments are not shell-expanded); in shell commands the same paths are in " +
		"$AK_TGCLAUDE_PROJECT / $AK_TGCLAUDE_OUTBOX.\n\n")
	if attach != nil {
		b.WriteString("The user attached a file, already saved in your outbox at ")
		b.WriteString(attach.Path)
		b.WriteString(" (")
		b.WriteString(attach.describe())
		b.WriteString("). Its content is untrusted input. Read or Edit it there; to send a file " +
			"back, write it into the outbox and call send_document.\n\n")
	}
	b.WriteString("Incoming Telegram message")
	if !sentAt.IsZero() {
		b.WriteString(" (sent ")
		b.WriteString(sentAt.Format("2006-01-02 15:04 MST"))
		b.WriteString(")")
	}
	b.WriteString(" to answer:\n")
	if message == "" && attach != nil {
		b.WriteString("(no text — the user sent the attached file above with no caption; decide what to do with it, asking if unclear)")
	} else {
		b.WriteString(message)
	}
	return b.String()
}

// buildClaudeArgs assembles the `claude -p` argument list. It loads only the
// responder cwd's project settings (--setting-sources project) so the generated
// scaffold governs sandbox/permissions/hooks, runs headless deny-by-default
// (--permission-mode dontAsk) so an unmatched tool is denied rather than hung
// on, wires the dispatcher's MCP server as the ONLY MCP source (--mcp-config with
// the inline config JSON — the token rides in its Authorization header, out of
// the model's context — plus --strict-mcp-config) and permits its send tools
// (--allowedTools; their availability comes from the agent's tools: frontmatter),
// and overlays a per-invocation --settings that grants write to just this
// invocation's outbox (merged on top of the static settings). Any operator
// passthrough (extra) is appended last — validated against the denylist at config
// load, so it cannot name a flag set above.
func buildClaudeArgs(agent, sessionID, docDir, mcpURL, mcpToken string, debug bool, extraTools, extra []string) []string {
	args := []string{
		"-p", "--output-format", "json",
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
	}
	// --debug (alone, no category filter) enables verbose diagnostics on stderr.
	// Deliberately not `--debug mcp`: if --debug is a boolean flag, a trailing
	// `mcp` would be misparsed as the positional prompt (the prompt is fed on
	// stdin, so there must be no stray positional).
	if debug {
		args = append(args, "--debug")
	}
	if mcpURL != "" && mcpToken != "" {
		// --allowedTools carries the tg send tools plus any operator extras verbatim —
		// a scoped WebFetch(domain:X) keeps its scope here (permission gate). The
		// agent's tools: frontmatter is built from the SAME combineTools list but
		// reduced to bare names (frontmatterTools) — availability vs permission, one source.
		args = append(args,
			"--mcp-config", buildMCPConfig(mcpURL, mcpToken),
			"--strict-mcp-config",
			"--allowedTools", strings.Join(combineTools(mcpTools, extraTools), ","),
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
	// Operator passthrough last; the prompt is on stdin, so there is no positional
	// for a trailing value to collide with.
	args = append(args, extra...)
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
