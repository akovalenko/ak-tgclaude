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
		FilePath string `json:"file_path"` // Read/Write/Edit
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

// runHookPreToolUse gates the responder's tool calls. It DENIES the two things
// that are out of contract — reading the token file (any tool) and running an
// unsandboxed Bash command — and DEFERS everything else to the normal
// permission + sandbox flow (so the per-invocation Write grant, the static Read
// allow, and dontAsk decide). It never blanket-"allow"s, which would override
// those layers.
func runHookPreToolUse(args []string) {
	fs := flag.NewFlagSet("hook pretooluse", flag.ContinueOnError)
	var deny multiFlag
	fs.Var(&deny, "deny-read", "path the responder must not read (repeatable)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	var in preToolUseInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		// Fail safe: on unparseable input, deny rather than let a tool through.
		emitDecision("deny", "ak-tgclaude hook: could not parse PreToolUse input")
		return
	}

	switch decision, reason := decidePreToolUse(&in, deny); decision {
	case "":
		os.Exit(0) // defer: no output => normal permission/sandbox flow applies
	default:
		emitDecision(decision, reason)
	}
}

// decidePreToolUse returns "deny", "allow", or "" (defer). It denies a
// token-file touch (any tool) and an unsandboxed Bash command; it explicitly
// allows sandboxed Bash; everything else defers.
func decidePreToolUse(in *preToolUseInput, deny []string) (decision, reason string) {
	if path, blocked := denyReason(in, deny); blocked {
		return "deny", "ak-tgclaude hook: access to a protected path is denied: " + path
	}
	if in.ToolName == "Bash" {
		if in.ToolInput.DangerouslyDisableSandbox {
			return "deny", "ak-tgclaude hook: unsandboxed Bash is not permitted (read-only, sandboxed inspection only)"
		}
		return "allow", "ak-tgclaude hook: sandboxed Bash allowed"
	}
	return "", ""
}

// denyReason reports whether the tool call touches a protected path.
func denyReason(in *preToolUseInput, deny []string) (string, bool) {
	if len(deny) == 0 {
		return "", false
	}
	switch in.ToolName {
	case "Read", "Edit", "Write", "NotebookEdit":
		if p := matchDenied(in.ToolInput.FilePath, deny); p != "" {
			return p, true
		}
	case "Bash":
		// Best-effort: block a command that mentions a protected path by name.
		for _, d := range deny {
			if d != "" && strings.Contains(in.ToolInput.Command, d) {
				return d, true
			}
		}
	}
	return "", false
}

// matchDenied returns the protected path if file (resolved) equals or sits under
// one of the deny paths.
func matchDenied(file string, deny []string) string {
	if file == "" {
		return ""
	}
	abs := file
	if a, err := filepath.Abs(file); err == nil {
		abs = a
	}
	abs = filepath.Clean(abs)
	for _, d := range deny {
		if d == "" {
			continue
		}
		dc := d
		if a, err := filepath.Abs(d); err == nil {
			dc = a
		}
		dc = filepath.Clean(dc)
		if abs == dc || strings.HasPrefix(abs, dc+string(os.PathSeparator)) {
			return d
		}
	}
	return ""
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
