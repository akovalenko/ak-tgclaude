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

func TestDenyReasonBash(t *testing.T) {
	deny := []string{"/home/bot/bot.toml"}
	in := &preToolUseInput{ToolName: "Bash"}
	in.ToolInput.Command = "cat /home/bot/bot.toml | base64"
	if _, blocked := denyReason(in, deny); !blocked {
		t.Errorf("bash referencing the token path should be denied")
	}
	in.ToolInput.Command = "grep foo main.go"
	if _, blocked := denyReason(in, deny); blocked {
		t.Errorf("unrelated bash should be allowed")
	}
}

func TestDenyReasonNoDenyList(t *testing.T) {
	in := &preToolUseInput{ToolName: "Read"}
	in.ToolInput.FilePath = "/anything"
	if _, blocked := denyReason(in, nil); blocked {
		t.Errorf("with no deny list nothing is blocked")
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
