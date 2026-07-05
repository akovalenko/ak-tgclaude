package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// runHook dispatches the hook sub-mode. Only "pretooluse" exists; it exits on
// its own (the decision JSON + exit status IS the hook protocol), so only the
// sub-mode dispatch reports errors back to main.
func runHook(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("missing sub-mode (want: pretooluse)")}
	}
	switch args[0] {
	case "pretooluse":
		runHookPreToolUse(args[1:])
		return nil
	default:
		return usageError{fmt.Errorf("unknown sub-mode %q (want: pretooluse)", args[0])}
	}
}

// preToolUseInput is the JSON Claude Code passes on stdin before a tool runs.
type preToolUseInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath string `json:"file_path"` // Read/Write/Edit/NotebookEdit
		Command  string `json:"command"`   // Bash
		// DangerouslyDisableSandbox is set by the model only when it wants a Bash
		// command to run OUTSIDE the sandbox; absent/false means sandboxed. (This
		// field is undocumented but real — the harness approve-hooks rely on it.)
		DangerouslyDisableSandbox bool `json:"dangerouslyDisableSandbox"`
	} `json:"tool_input"`
}

// preToolUseDecision is the JSON the hook prints to allow/deny a tool call.
type preToolUseDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"` // allow|deny|ask
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// filePolicy is the responder's file-tool access policy. The hook is the single
// authority for the file tools (permissions carry only the deferred tools):
//
//	Read           -> allow under readRoots, else deny
//	Edit/Write/...  -> allow under writeRoots (the outbox + tmp), else deny
//	deny (token)    -> deny for any file tool (checked first, wins over the above)
//
// readRoots is a superset of writeRoots — the responder can read anything it can
// write (so it can iterate on files it authored) plus the project; writeRoots is
// the outbox (the project stays read-only). See envFilePolicy.
type filePolicy struct {
	deny       []string // protected paths (token); highest priority
	readRoots  []string // Read allowed under these (project + writeRoots)
	writeRoots []string // Edit/Write/NotebookEdit allowed under these
	bangBug    bool     // deny sandboxed Bash whose command contains `\!` (bug #64301)
}

// envFilePolicy resolves the policy from the responder's env at hook time:
// writeRoots = $AK_TGCLAUDE_OUTBOX; readRoots = $AK_TGCLAUDE_PROJECT + writeRoots
// (read what you can write, plus the project). The responder runs with
// TMPDIR=$AK_TGCLAUDE_OUTBOX, so temp lands under the outbox too — /tmp/claude-<uid>
// is no longer a scratch root.
func envFilePolicy(deny []string) filePolicy {
	writeRoots := envRoots(outboxEnv)
	readRoots := append(envRoots(projectEnv), writeRoots...)
	// The transcript read scope (a chat's own subdir, or the whole root for the
	// owner) is readable by the Read tool too; an unset env adds nothing.
	readRoots = append(readRoots, envRoots(transcriptEnv)...)
	// The usage-log file is readable by the Read tool too, but ONLY when the env is
	// set — which the dispatcher does solely for the owner's invocation. A non-owner
	// has no such env (adds nothing) AND a sandbox denyRead, so it stays closed.
	readRoots = append(readRoots, envRoots(usageLogEnv)...)
	return filePolicy{deny: deny, readRoots: readRoots, writeRoots: writeRoots}
}

// runHookPreToolUse gates the responder's tool calls: it path-scopes the file
// tools per filePolicy, allows sandboxed Bash / denies unsandboxed Bash (and,
// with --bang-bug, sandboxed Bash whose command carries the corrupted `\!`), and
// DEFERS everything else (Grep/Glob/Skill/…) to the permission layer. A Bash
// read of the token is masked by the sandbox's credentials.files deny-read, not
// by this hook.
func runHookPreToolUse(args []string) {
	fs := flag.NewFlagSet("hook pretooluse", flag.ContinueOnError)
	var deny multiFlag
	fs.Var(&deny, "deny-read", "path the responder must not read (repeatable)")
	bangBug := fs.Bool("bang-bug", false, `deny sandboxed Bash whose command contains \! (bug #64301: the sandbox corrupts the bang char)`)
	logFile := fs.String("log-file", "", "append every PreToolUse call (tool, decision, full input) to this file")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Read the whole input so it can be both decoded AND logged raw (the typed
	// struct drops fields — e.g. WebFetch's url — that are useful for designing gates).
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		emitDecision("deny", "ak-tgclaude hook: could not read PreToolUse input")
		return
	}
	var in preToolUseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		// Fail safe: on unparseable input, deny rather than let a tool through.
		emitDecision("deny", "ak-tgclaude hook: could not parse PreToolUse input")
		return
	}

	pol := envFilePolicy(deny)
	pol.bangBug = *bangBug
	decision, reason := decidePreToolUse(&in, pol)
	if *logFile != "" {
		verdict := decision
		if verdict == "" {
			verdict = "defer"
		}
		// Full input, no length cap (the tail — e.g. a WebFetch url — is the point);
		// newlines flattened so each call is one line. File, not stderr: Claude Code
		// does not surface a hook's stderr to the dispatcher log even under --debug
		// (verified 2026-07-03).
		line := fmt.Sprintf("%s -> %s (%s) input=%s", in.ToolName, verdict, reason, strings.ReplaceAll(string(raw), "\n", " "))
		appendHookLog(*logFile, line)
	}
	switch decision {
	case "":
		os.Exit(0) // defer: no output => normal permission/sandbox flow applies
	default:
		emitDecision(decision, reason)
	}
}

// appendHookLog appends one timestamped line to path (created if missing). Claude
// Code does NOT surface a hook's stderr to the dispatcher log, so a file is how
// PreToolUse calls are observed. Best-effort: an open error is swallowed —
// diagnostic logging must never break the gate. Concurrent hooks are safe: an
// O_APPEND write this short is atomic on POSIX.
func appendHookLog(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.New(f, "", log.LstdFlags).Println(line)
	_ = f.Close() // unbuffered *os.File — Println already wrote; close error is moot
}

// decidePreToolUse returns "deny", "allow", or "" (defer).
func decidePreToolUse(in *preToolUseInput, pol filePolicy) (decision, reason string) {
	// Token guard first: a file tool touching a protected path is denied even if
	// that path happens to sit under the project (checked before the read allow).
	if p, ok := underAny(in.ToolInput.FilePath, pol.deny); ok {
		return "deny", "ak-tgclaude hook: access to a protected path is denied: " + p
	}

	switch in.ToolName {
	case "Bash":
		if in.ToolInput.DangerouslyDisableSandbox {
			return "deny", "ak-tgclaude hook: unsandboxed Bash is not permitted (read-only, sandboxed inspection only)"
		}
		// bug #64301: sandboxed Bash blind-escapes `!`→`\!`, silently corrupting
		// the command/output. The corrupted `\!` is the detectable signature; push
		// such work to a file (heredoc included), which the sandbox runs verbatim.
		if pol.bangBug && strings.Contains(in.ToolInput.Command, `\!`) {
			return "deny", "ak-tgclaude hook: sandboxed Bash corrupts the bang char (bug #64301) — write the script to a file and run the file"
		}
		return "allow", "ak-tgclaude hook: sandboxed Bash allowed"

	case "Read":
		if _, ok := underAny(in.ToolInput.FilePath, pol.readRoots); ok {
			return "allow", "ak-tgclaude hook: read within the project"
		}
		return "deny", "ak-tgclaude hook: read is limited to the project " +
			fmtRoots(pol.readRoots) + " — read other locations with sandboxed Bash"

	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		if _, ok := underAny(in.ToolInput.FilePath, pol.writeRoots); ok {
			return "allow", "ak-tgclaude hook: write within the outbox"
		}
		return "deny", "ak-tgclaude hook: write is limited to the outbox " + fmtRoots(pol.writeRoots)
	}

	return "", "" // defer (Grep/Glob/Skill/…)
}

// underAny returns the first root that file (resolved to an absolute, cleaned
// path) equals or sits under, and whether one matched.
func underAny(file string, roots []string) (string, bool) {
	if file == "" {
		return "", false
	}
	abs := absClean(file)
	for _, r := range roots {
		if r == "" {
			continue
		}
		rc := absClean(r)
		if abs == rc || strings.HasPrefix(abs, rc+string(os.PathSeparator)) {
			return r, true
		}
	}
	return "", false
}

// absClean resolves p to an absolute, cleaned path (best-effort).
func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

// envRoots returns the value of env var name as a one-element root list, or nil
// if it is unset/empty.
func envRoots(name string) []string {
	if v := os.Getenv(name); v != "" {
		return []string{v}
	}
	return nil
}

// fmtRoots renders roots for a deny reason.
func fmtRoots(roots []string) string {
	if len(roots) == 0 {
		return "(none configured)"
	}
	return "[" + strings.Join(roots, " ") + "]"
}

// emitDecision prints the PreToolUse decision JSON and exits 0.
func emitDecision(decision, reason string) {
	var out preToolUseDecision
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = decision
	out.HookSpecificOutput.PermissionDecisionReason = reason
	_ = json.NewEncoder(os.Stdout).Encode(&out)
	os.Exit(0)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
