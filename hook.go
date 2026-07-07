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
//	deny            -> deny for any file tool, first (host secrets, token, deny_reads)
//	Read            -> readAllow carve wins; else readDeny mask denies; else ALLOW
//	Edit/Write/...   -> allow under writeRoots (the outbox), else deny
//
// Read MIRRORS the sandbox: default-open, minus the masked roots (sibling outboxes,
// other chats' transcripts, a non-owner's usage log), with this invocation's own
// scopes (its outbox, its transcript scope, an owner's usage log) carved back in —
// exactly the sandbox's denyRead + allowRead. So the Read tool reaches precisely
// what sandboxed Bash reaches: no project confinement, no "denied here, cat it via
// Bash" theatre. Writes stay a strict allowlist (the outbox). See envFilePolicy.
type filePolicy struct {
	deny       []string // never read/written: host secrets, token, operator deny_reads; wins first
	readAllow  []string // Read carves (own outbox/transcript, owner usage log) — win over readDeny
	readDeny   []string // Read masks (sibling outboxes, other transcripts, non-owner usage log)
	writeRoots []string // Edit/Write/NotebookEdit allowed only under these (the outbox)
	bangBug    bool     // deny sandboxed Bash whose command contains `\!` (bug #64301)
	failClosed bool     // no/invalid policy: deny every file tool (Read is otherwise default-open)
}

// hookFilePolicy is the file-tool access policy the dispatcher computes and hands
// the hook as JSON in filePolicyEnv. It is the SINGLE definition of what the
// responder may read / write / never touch: the dispatcher derives it once (see
// (*claudeResponder).filePolicy) and projects it both here (the file tools) and
// into the sandbox settings (Bash, via allowWrite), so the two cannot drift — a
// the Read carves/masks mirror the sandbox's allowRead/denyRead, and writeRoots
// feeds allowWrite. JSON, not a separator-joined list, so a path holding any byte
// (including the ':' that would break a PATH-style list) round-trips exactly.
type hookFilePolicy struct {
	WriteRoots []string `json:"writeRoots"`          // Edit/Write allowed only under these (the outbox)
	ReadAllow  []string `json:"readAllow,omitempty"` // Read carves: own outbox/transcript, owner usage log
	ReadDeny   []string `json:"readDeny,omitempty"`  // Read masks: sibling outboxes, other transcripts, non-owner usage log
	Deny       []string `json:"deny,omitempty"`      // never read/written; wins first (host secrets, token, deny_reads)
}

// envFilePolicy decodes the hook's policy from filePolicyEnv. Read is default-OPEN
// in the mirror model, so a missing or malformed policy is treated as a hard fail:
// deny every file tool (a readDeny of "/" masks all absolute paths, writeRoots
// cleared) rather than expose the filesystem on a corrupt spawn.
func envFilePolicy() filePolicy {
	var w hookFilePolicy
	ok := false
	if v := os.Getenv(filePolicyEnv); v != "" {
		ok = json.Unmarshal([]byte(v), &w) == nil
	}
	if !ok {
		return filePolicy{failClosed: true}
	}
	return filePolicy{deny: w.Deny, readAllow: w.ReadAllow, readDeny: w.ReadDeny, writeRoots: w.WriteRoots}
}

// runHookPreToolUse gates the responder's tool calls: it path-scopes the file
// tools per filePolicy, allows sandboxed Bash / denies unsandboxed Bash (and,
// with --bang-bug, sandboxed Bash whose command carries the corrupted `\!`), and
// DEFERS everything else (Grep/Glob/Skill/…) to the permission layer. A Bash
// read of the token is masked by the sandbox's credentials.files deny-read, not
// by this hook.
func runHookPreToolUse(args []string) {
	fs := flag.NewFlagSet("hook pretooluse", flag.ContinueOnError)
	// The file-tool policy (read/write/deny roots) arrives via filePolicyEnv, not a
	// flag — a single dispatcher-computed source, see (*claudeResponder).filePolicy.
	// Only the behavioral knobs are flags.
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

	pol := envFilePolicy()
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
	fp := in.ToolInput.FilePath
	// The file tools FOLLOW symlinks, and an "allow" from this hook SHORT-CIRCUITS
	// Claude Code's own (symlink-resolving) permission check — so a link a responder
	// plants in its writable outbox would redirect a read/write to a denied target
	// unless we resolve it here too. Match the symlink-resolved path against
	// symlink-resolved roots: resolving the roots as well keeps the own-scope carve
	// intact when a path PREFIX (e.g. /run -> /var/run, /tmp -> /private/tmp) is itself
	// a symlink, while a link that resolves OUT of the own scope is caught by the mask.
	resolved := resolveForPolicy(fp)

	// Absolute deny first: host secrets, the token, operator deny_reads — never read
	// or written by any file tool, wins over every allow below. Checked on the lexical
	// path AND on the resolved path (a symlink that lands on a denied target).
	if p, ok := underAny(fp, pol.deny); ok {
		return "deny", "ak-tgclaude hook: access to a protected path is denied: " + p
	}
	if p, ok := underAny(resolved, resolveRoots(pol.deny)); ok {
		return "deny", "ak-tgclaude hook: access to a protected path is denied (via symlink): " + p
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
		if pol.failClosed {
			return "deny", "ak-tgclaude hook: no file-tool policy present — denying (fail-closed)"
		}
		// Mirror the sandbox: an own-scope carve wins, then a masked root denies, else
		// read is open — exactly what sandboxed Bash can read, so no project confinement
		// and no Bash-cat detour for an ordinary external file. Match the RESOLVED path
		// so a symlink planted in the own outbox cannot launder a read of a sibling
		// outbox / another chat's transcript: the own-scope carve (more specific) still
		// wins over the mask, but a link that resolves OUT of it falls through to it.
		if _, ok := underAny(resolved, resolveRoots(pol.readAllow)); ok {
			return "allow", "ak-tgclaude hook: read allowed (own scope)"
		}
		if r, ok := underAny(resolved, resolveRoots(pol.readDeny)); ok {
			return "deny", "ak-tgclaude hook: read of a masked location is denied: " + r
		}
		return "allow", "ak-tgclaude hook: read allowed"

	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		// Write is a strict allowlist AND the bytes must land physically in the outbox:
		// match the resolved path so a symlink already sitting in the outbox cannot
		// redirect the write to a host file (e.g. ~/.bashrc). A fresh (not-yet-created)
		// target resolves through its outbox parent and still matches.
		if _, ok := underAny(resolved, resolveRoots(pol.writeRoots)); ok {
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

// resolveForPolicy returns file's symlink-resolved absolute path for policy
// matching. The file tools follow symlinks, so the deny/mask/allow roots must be
// matched against the path the kernel will actually reach, not the lexical one. A
// path that does not exist yet (a fresh Write target) has its existing parent
// resolved and the final element re-attached, so a symlinked directory prefix is
// still caught while a legitimate new outbox file still resolves under the outbox.
func resolveForPolicy(file string) string {
	if file == "" {
		return ""
	}
	abs := absClean(file)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, base := filepath.Split(abs)
	if rd, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(rd, base)
	}
	return abs
}

// resolveRoots returns roots with each entry symlink-resolved (best-effort), so a
// resolved file path is matched against resolved roots — consistent even when a
// path prefix (e.g. /run -> /var/run, /tmp -> /private/tmp) is itself a symlink. A
// root that does not resolve (nonexistent in a test, say) falls back to its lexical
// clean form, so the match degrades to the original lexical behavior.
func resolveRoots(roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = resolveForPolicy(r)
	}
	return out
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
