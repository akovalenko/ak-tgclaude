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

// runHookPreToolUse gates the responder's tool calls. It is the primary token
// guard: it denies any Read (or file-touching tool) of a --deny-read path and
// any Bash command that references one (best-effort; the sandbox's
// credentials.files deny-read is the authoritative backstop against obfuscation).
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

	if path, blocked := denyReason(&in, deny); blocked {
		emitDecision("deny", "ak-tgclaude hook: access to a protected path is denied: "+path)
		return
	}
	// Defer to the normal permission flow for everything else.
	emitDecision("allow", "")
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
