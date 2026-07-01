package main

import (
	"path/filepath"
	"testing"
)

func TestDenyReasonRead(t *testing.T) {
	deny := []string{"/home/bot/bot.toml"}

	// Read of the exact token file -> denied.
	in := &preToolUseInput{ToolName: "Read"}
	in.ToolInput.FilePath = "/home/bot/bot.toml"
	if p, blocked := denyReason(in, deny); !blocked || p != "/home/bot/bot.toml" {
		t.Errorf("read of token should be denied, got %q blocked=%v", p, blocked)
	}

	// Read of an unrelated file -> allowed.
	in.ToolInput.FilePath = "/home/bot/code/main.go"
	if _, blocked := denyReason(in, deny); blocked {
		t.Errorf("read of unrelated file should be allowed")
	}
}

func TestDenyReasonReadUnderDir(t *testing.T) {
	deny := []string{"/home/bot/secrets"}
	in := &preToolUseInput{ToolName: "Read"}
	in.ToolInput.FilePath = "/home/bot/secrets/token.txt"
	if _, blocked := denyReason(in, deny); !blocked {
		t.Errorf("read under a denied dir should be denied")
	}
	// A sibling that merely shares a prefix must NOT be denied.
	in.ToolInput.FilePath = "/home/bot/secrets-public/readme.md"
	if _, blocked := denyReason(in, deny); blocked {
		t.Errorf("prefix-only sibling should not be denied")
	}
}

func TestDenyReasonDoesNotGateBash(t *testing.T) {
	// The hook no longer string-matches Bash commands for the token — the
	// sandbox's credentials.files deny-read is the authoritative guard (masks
	// the file), so denyReason leaves Bash to the sandbox.
	deny := []string{"/home/bot/bot.toml"}
	in := &preToolUseInput{ToolName: "Bash"}
	in.ToolInput.Command = "cat /home/bot/bot.toml | base64"
	if _, blocked := denyReason(in, deny); blocked {
		t.Errorf("denyReason must not gate Bash (the sandbox does)")
	}
}

func TestDenyReasonNoDenyList(t *testing.T) {
	in := &preToolUseInput{ToolName: "Read"}
	in.ToolInput.FilePath = "/anything"
	if _, blocked := denyReason(in, nil); blocked {
		t.Errorf("with no deny list nothing is blocked")
	}
}

func bashInput(cmd string, disableSandbox bool) *preToolUseInput {
	in := &preToolUseInput{ToolName: "Bash"}
	in.ToolInput.Command = cmd
	in.ToolInput.DangerouslyDisableSandbox = disableSandbox
	return in
}

func TestDecideBashSandbox(t *testing.T) {
	// Sandboxed Bash -> allow.
	if d, _ := decidePreToolUse(bashInput("grep foo .", false), nil); d != "allow" {
		t.Errorf("sandboxed Bash => %q, want allow", d)
	}
	// Unsandboxed Bash -> deny.
	if d, r := decidePreToolUse(bashInput("git pull", true), nil); d != "deny" {
		t.Errorf("unsandboxed Bash => %q (%s), want deny", d, r)
	}
	// A sandboxed Bash that names the token is ALLOWED by the hook — the sandbox's
	// credentials.files deny-read masks the file (ENOENT), so the hook needn't
	// (and no longer does) string-match the command.
	if d, _ := decidePreToolUse(bashInput("cat /cfg/bot.toml", false), []string{"/cfg/bot.toml"}); d != "allow" {
		t.Errorf("sandboxed Bash naming the token => %q, want allow (sandbox masks the file)", d)
	}
}

func TestDecideDefersNonBash(t *testing.T) {
	// A non-token Read defers (empty decision) — permissions/sandbox decide.
	read := &preToolUseInput{ToolName: "Read"}
	read.ToolInput.FilePath = "/home/bot/code/main.go"
	if d, _ := decidePreToolUse(read, []string{"/cfg/bot.toml"}); d != "" {
		t.Errorf("non-token Read => %q, want defer (empty)", d)
	}
	// A non-token Write also defers (the per-invocation Write grant governs it,
	// NOT the hook — the hook must not blanket-allow).
	write := &preToolUseInput{ToolName: "Write"}
	write.ToolInput.FilePath = "/etc/passwd"
	if d, _ := decidePreToolUse(write, nil); d != "" {
		t.Errorf("non-token Write => %q, want defer (empty)", d)
	}
	// But a Write to the token file is denied.
	wtok := &preToolUseInput{ToolName: "Write"}
	wtok.ToolInput.FilePath = "/cfg/bot.toml"
	if d, _ := decidePreToolUse(wtok, []string{"/cfg/bot.toml"}); d != "deny" {
		t.Errorf("Write to token => %q, want deny", d)
	}
}

func TestMatchDeniedRelative(t *testing.T) {
	// Relative deny + relative file should still match after Abs/Clean.
	rel := "sub/secret.txt"
	abs, _ := filepath.Abs(rel)
	if matchDenied(abs, []string{rel}) == "" {
		t.Errorf("relative deny path should match its absolute file")
	}
}
