package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setFilePolicyEnv marshals p to JSON and sets it as the hook's policy env, as the
// dispatcher does when spawning the responder.
func setFilePolicyEnv(t *testing.T, p hookFilePolicy) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(filePolicyEnv, string(b))
}

func fileInput(tool, path string) *preToolUseInput {
	in := &preToolUseInput{ToolName: tool}
	in.ToolInput.FilePath = path
	return in
}

func bashInput(cmd string, disableSandbox bool) *preToolUseInput {
	in := &preToolUseInput{ToolName: "Bash"}
	in.ToolInput.Command = cmd
	in.ToolInput.DangerouslyDisableSandbox = disableSandbox
	return in
}

var testPolicy = filePolicy{
	deny:       []string{"/cfg/bot.toml"},
	readRoots:  []string{"/proj"},
	writeRoots: []string{"/run/out/outbox-A1"},
}

func TestDecideReadScopedToProject(t *testing.T) {
	if d, _ := decidePreToolUse(fileInput("Read", "/proj/main.go"), testPolicy); d != "allow" {
		t.Errorf("read within project => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Read", "/proj"), testPolicy); d != "allow" {
		t.Errorf("read of project root => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Read", "/etc/passwd"), testPolicy); d != "deny" {
		t.Errorf("read outside project => %q, want deny", d)
	}
	// A sibling that merely shares a prefix is NOT inside the project.
	if d, _ := decidePreToolUse(fileInput("Read", "/proj-secret/x"), testPolicy); d != "deny" {
		t.Errorf("prefix-sibling read => %q, want deny", d)
	}
}

func TestDecideWriteScopedToOutbox(t *testing.T) {
	if d, _ := decidePreToolUse(fileInput("Write", "/run/out/outbox-A1/reply.html"), testPolicy); d != "allow" {
		t.Errorf("write in outbox => %q, want allow", d)
	}
	// Edit / MultiEdit / NotebookEdit follow the same write policy.
	for _, tool := range []string{"Edit", "MultiEdit", "NotebookEdit"} {
		if d, _ := decidePreToolUse(fileInput(tool, "/run/out/outbox-A1/x"), testPolicy); d != "allow" {
			t.Errorf("%s in outbox => %q, want allow", tool, d)
		}
	}
	// Writing into the project (read-only), /tmp (no longer a scratch root), or
	// anywhere else is denied.
	for _, p := range []string{"/proj/main.go", "/tmp/claude-1000/scratch.md", "/etc/cron.d/x"} {
		if d, _ := decidePreToolUse(fileInput("Write", p), testPolicy); d != "deny" {
			t.Errorf("write %q => %q, want deny", p, d)
		}
	}
}

func TestDecideTokenWinsOverProject(t *testing.T) {
	// The token sits under the project, but the deny check runs first.
	pol := filePolicy{deny: []string{"/proj/secret.toml"}, readRoots: []string{"/proj"}}
	if d, _ := decidePreToolUse(fileInput("Read", "/proj/secret.toml"), pol); d != "deny" {
		t.Errorf("token under project => %q, want deny", d)
	}
	// A normal project file is still allowed.
	if d, _ := decidePreToolUse(fileInput("Read", "/proj/main.go"), pol); d != "allow" {
		t.Errorf("project file => %q, want allow", d)
	}
}

func TestDecideBash(t *testing.T) {
	if d, _ := decidePreToolUse(bashInput("grep foo /proj", false), testPolicy); d != "allow" {
		t.Errorf("sandboxed Bash => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(bashInput("git pull", true), testPolicy); d != "deny" {
		t.Errorf("unsandboxed Bash => %q, want deny", d)
	}
}

func TestDecideBashBangBug(t *testing.T) {
	bangPol := testPolicy
	bangPol.bangBug = true

	// With the guard on, a corrupted `\!` in a sandboxed command is denied.
	if d, r := decidePreToolUse(bashInput(`echo "done\!"`, false), bangPol); d != "deny" || !strings.Contains(r, "#64301") {
		t.Errorf("bang command => %q / %q, want deny citing #64301", d, r)
	}
	// A plain command with no bang is still allowed.
	if d, _ := decidePreToolUse(bashInput("go build ./...", false), bangPol); d != "allow" {
		t.Errorf("no-bang command => %q, want allow", d)
	}
	// A bare `!` (not the corrupted `\!`) is not the signature — only `\!` trips it.
	if d, _ := decidePreToolUse(bashInput("test ! -f x", false), bangPol); d != "allow" {
		t.Errorf("bare bang => %q, want allow (only \\! is the signature)", d)
	}
	// Guard off (default): the same corrupted command is allowed through.
	if d, _ := decidePreToolUse(bashInput(`echo "done\!"`, false), testPolicy); d != "allow" {
		t.Errorf("bang with guard off => %q, want allow", d)
	}
}

func TestDecideDefersOtherTools(t *testing.T) {
	for _, tool := range []string{"Grep", "Glob", "Skill", "WebFetch"} {
		if d, _ := decidePreToolUse(&preToolUseInput{ToolName: tool}, testPolicy); d != "" {
			t.Errorf("%s => %q, want defer (empty)", tool, d)
		}
	}
}

func TestEnvFilePolicy(t *testing.T) {
	setFilePolicyEnv(t, hookFilePolicy{
		WriteRoots: []string{"/run/out/o1"},
		ReadRoots:  []string{"/proj", "/run/out/o1"},
		Deny:       []string{"/cfg/bot.toml"},
	})
	pol := envFilePolicy()

	// Read is allowed in the project AND the writable area (read what you write,
	// so authoring can iterate).
	for _, p := range []string{"/proj/main.go", "/run/out/o1/draft.md"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), pol); d != "allow" {
			t.Errorf("read %q => %q, want allow", p, d)
		}
	}
	// Write is allowed only in the outbox, not the (read-only) project.
	if d, _ := decidePreToolUse(fileInput("Write", "/run/out/o1/draft.md"), pol); d != "allow" {
		t.Errorf("write outbox => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Write", "/proj/main.go"), pol); d != "deny" {
		t.Errorf("write project => %q, want deny (read-only)", d)
	}
	// /tmp/claude-<uid> is no longer a scratch root: read and write are denied
	// (temp now lives under the outbox via TMPDIR).
	const tmp = "/tmp/claude-1000/scratch.txt"
	if d, _ := decidePreToolUse(fileInput("Read", tmp), pol); d != "deny" {
		t.Errorf("read /tmp => %q, want deny", d)
	}
	if d, _ := decidePreToolUse(fileInput("Write", tmp), pol); d != "deny" {
		t.Errorf("write /tmp => %q, want deny", d)
	}
	// Token denied first even though it isn't under any root.
	if d, _ := decidePreToolUse(fileInput("Read", "/cfg/bot.toml"), pol); d != "deny" {
		t.Errorf("token read => %q, want deny", d)
	}
}

func TestEnvFilePolicyFailSafe(t *testing.T) {
	// Missing or malformed policy => empty roots => every file tool denied (Bash is
	// governed by the sandbox, not this hook, so it is unaffected).
	for _, v := range []string{"", "{not json"} {
		t.Setenv(filePolicyEnv, v)
		pol := envFilePolicy()
		if d, _ := decidePreToolUse(fileInput("Read", "/proj/main.go"), pol); d != "deny" {
			t.Errorf("policy=%q: read => %q, want deny (fail-safe)", v, d)
		}
		if d, _ := decidePreToolUse(fileInput("Write", "/whatever"), pol); d != "deny" {
			t.Errorf("policy=%q: write => %q, want deny (fail-safe)", v, d)
		}
		if d, _ := decidePreToolUse(bashInput("echo hi", false), pol); d != "allow" {
			t.Errorf("policy=%q: sandboxed Bash => %q, want allow (sandbox governs it)", v, d)
		}
	}
}

func TestEnvFilePolicyColonPath(t *testing.T) {
	// JSON round-trips a path holding a ':' exactly — the reason we do not join the
	// list PATH-style (which would split such a path).
	const p = "/run/out/o:1"
	setFilePolicyEnv(t, hookFilePolicy{WriteRoots: []string{p}, ReadRoots: []string{p}})
	pol := envFilePolicy()
	if d, _ := decidePreToolUse(fileInput("Write", p+"/draft.md"), pol); d != "allow" {
		t.Errorf("write under colon-path => %q, want allow (path must round-trip intact)", d)
	}
}

func TestEnvFilePolicyTranscriptScope(t *testing.T) {
	setFilePolicyEnv(t, hookFilePolicy{
		WriteRoots: []string{"/run/out/o1"},
		ReadRoots:  []string{"/proj", "/run/out/o1", "/s/transcripts/42"},
	})
	pol := envFilePolicy()

	// Read is allowed under this chat's own transcript scope...
	if d, _ := decidePreToolUse(fileInput("Read", "/s/transcripts/42/2026-07-04.jsonl"), pol); d != "allow" {
		t.Errorf("read own transcript => %q, want allow", d)
	}
	// ...but a sibling chat's dir is not under the scope => deny (no cross-chat read).
	if d, _ := decidePreToolUse(fileInput("Read", "/s/transcripts/99/2026-07-04.jsonl"), pol); d != "deny" {
		t.Errorf("read sibling transcript => %q, want deny", d)
	}
	// The transcript is read-only: a Write is denied.
	if d, _ := decidePreToolUse(fileInput("Write", "/s/transcripts/42/x"), pol); d != "deny" {
		t.Errorf("write transcript => %q, want deny", d)
	}
}

func TestUnderAny(t *testing.T) {
	if _, ok := underAny("/a/b/c", []string{"/a/b"}); !ok {
		t.Errorf("file under root should match")
	}
	if _, ok := underAny("/a/b", []string{"/a/b"}); !ok {
		t.Errorf("file equal to root should match")
	}
	if _, ok := underAny("/a/bc", []string{"/a/b"}); ok {
		t.Errorf("prefix-only sibling must not match")
	}
	if _, ok := underAny("", []string{"/a"}); ok {
		t.Errorf("empty file must not match")
	}
	if _, ok := underAny("/x", nil); ok {
		t.Errorf("no roots must not match")
	}
	// Relative file resolves against cwd, then matches an absolute root that
	// contains cwd (best-effort abs/clean).
	abs, _ := filepath.Abs("sub/f")
	if _, ok := underAny(abs, []string{filepath.Dir(filepath.Dir(abs))}); !ok {
		t.Errorf("abs/clean matching failed")
	}
}

func TestAppendHookLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pretooluse.log")
	appendHookLog(p, "first")
	appendHookLog(p, "second")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "first") || !strings.Contains(s, "second") {
		t.Errorf("both entries should be appended: %q", s)
	}
	if n := strings.Count(s, "\n"); n != 2 {
		t.Errorf("expected two log lines, got %d: %q", n, s)
	}
	// A bad path must be swallowed — diagnostic logging never breaks the gate.
	appendHookLog(filepath.Join(t.TempDir(), "no-such-dir", "x.log"), "ignored")
}
