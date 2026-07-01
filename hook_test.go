package main

import (
	"path/filepath"
	"testing"
)

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
	writeRoots: []string{"/run/out/outbox-A1", "/tmp/claude-1000"},
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

func TestDecideWriteScopedToOutboxAndTmp(t *testing.T) {
	for _, p := range []string{"/run/out/outbox-A1/reply.html", "/tmp/claude-1000/scratch.md"} {
		if d, _ := decidePreToolUse(fileInput("Write", p), testPolicy); d != "allow" {
			t.Errorf("write %q => %q, want allow", p, d)
		}
	}
	// Edit / MultiEdit / NotebookEdit follow the same write policy.
	for _, tool := range []string{"Edit", "MultiEdit", "NotebookEdit"} {
		if d, _ := decidePreToolUse(fileInput(tool, "/run/out/outbox-A1/x"), testPolicy); d != "allow" {
			t.Errorf("%s in outbox => %q, want allow", tool, d)
		}
	}
	// Writing into the project (read-only) or anywhere else is denied.
	for _, p := range []string{"/proj/main.go", "/etc/cron.d/x"} {
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

func TestDecideDefersOtherTools(t *testing.T) {
	for _, tool := range []string{"Grep", "Glob", "Skill", "WebFetch"} {
		if d, _ := decidePreToolUse(&preToolUseInput{ToolName: tool}, testPolicy); d != "" {
			t.Errorf("%s => %q, want defer (empty)", tool, d)
		}
	}
}

func TestEnvFilePolicy(t *testing.T) {
	t.Setenv(projectEnv, "/proj")
	t.Setenv(outboxEnv, "/run/out/o1")
	pol := envFilePolicy([]string{"/cfg/bot.toml"})
	tmp := sandboxTmpDir()

	// Read is allowed in the project AND the writable areas (read what you write,
	// so authoring can iterate).
	for _, p := range []string{"/proj/main.go", "/run/out/o1/draft.md", tmp + "/scratch.txt"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), pol); d != "allow" {
			t.Errorf("read %q => %q, want allow", p, d)
		}
	}
	// Write is allowed only in the writable areas, not the (read-only) project.
	if d, _ := decidePreToolUse(fileInput("Write", "/run/out/o1/draft.md"), pol); d != "allow" {
		t.Errorf("write outbox => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Edit", tmp+"/scratch.txt"), pol); d != "allow" {
		t.Errorf("edit tmp => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Write", "/proj/main.go"), pol); d != "deny" {
		t.Errorf("write project => %q, want deny (read-only)", d)
	}
	// Token denied first even though it isn't under any root.
	if d, _ := decidePreToolUse(fileInput("Read", "/cfg/bot.toml"), pol); d != "deny" {
		t.Errorf("token read => %q, want deny", d)
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
