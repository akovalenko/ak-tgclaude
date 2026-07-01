package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runHook dispatches the hook sub-mode. Only "pretooluse" exists.
func runHook(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ak-tgclaude: hook: missing sub-mode (want: pretooluse)")
		os.Exit(2)
	}
	switch args[0] {
	case "pretooluse":
		runHookPreToolUse(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ak-tgclaude: hook: unknown sub-mode %q (want: pretooluse)\n", args[0])
		os.Exit(2)
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
// the outbox + tmp (the project stays read-only). See envFilePolicy.
type filePolicy struct {
	deny       []string // protected paths (token); highest priority
	readRoots  []string // Read allowed under these (project + writeRoots)
	writeRoots []string // Edit/Write/NotebookEdit allowed under these
	bangBug    bool     // deny sandboxed Bash whose command contains `\!` (bug #64301)
}

// envFilePolicy resolves the policy from the responder's env at hook time:
// writeRoots = $AK_TGCLAUDE_OUTBOX + the sandbox tmp dir; readRoots =
// $AK_TGCLAUDE_PROJECT + writeRoots (read what you can write, plus the project).
func envFilePolicy(deny []string) filePolicy {
	writeRoots := append(envRoots(outboxEnv), sandboxTmpDir())
	readRoots := append(envRoots(projectEnv), writeRoots...)
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
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	var in preToolUseInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		// Fail safe: on unparseable input, deny rather than let a tool through.
		emitDecision("deny", "ak-tgclaude hook: could not parse PreToolUse input")
		return
	}

	pol := envFilePolicy(deny)
	pol.bangBug = *bangBug
	switch decision, reason := decidePreToolUse(&in, pol); decision {
	case "":
		os.Exit(0) // defer: no output => normal permission/sandbox flow applies
	default:
		emitDecision(decision, reason)
	}
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
			return "allow", "ak-tgclaude hook: write within the outbox/tmp"
		}
		return "deny", "ak-tgclaude hook: write is limited to the outbox and tmp " + fmtRoots(pol.writeRoots)
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

// sandboxTmpDir is the per-uid temp the command sandbox makes writable by
// default (/tmp/claude-<uid>) — the responder's scratch area.
func sandboxTmpDir() string {
	return fmt.Sprintf("/tmp/claude-%d", os.Getuid())
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
