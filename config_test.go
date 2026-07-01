package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigAccessFlags(t *testing.T) {
	c, err := parseConfig([]string{"--allow-user", "1", "--allow-user", "2", "--open"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedUsers) != 2 || c.AllowedUsers[0] != 1 || c.AllowedUsers[1] != 2 {
		t.Errorf("--allow-user not collected: %v", c.AllowedUsers)
	}
	if !c.Open {
		t.Error("--open should set Open")
	}
}

func TestParseConfigAllowUserInvalid(t *testing.T) {
	if _, err := parseConfig([]string{"--allow-user", "notanumber"}); err == nil {
		t.Error("non-numeric --allow-user should error")
	}
}

func TestParseConfigWireSkills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("wire_skills = [\"/lib/a\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--wire-skill", "~/b", "--wire-skill", "/lib/c"})
	if err != nil {
		t.Fatal(err)
	}
	// Additive: file [/lib/a] + flags [~/b, /lib/c].
	if len(c.WireSkills) != 3 {
		t.Fatalf("expected 3 wire skills, got %v", c.WireSkills)
	}
	if c.WireSkills[0] != "/lib/a" || c.WireSkills[2] != "/lib/c" {
		t.Errorf("wire skills order/merge wrong: %v", c.WireSkills)
	}
	// A leading ~ is expanded (like project/cwd).
	home, _ := os.UserHomeDir()
	if c.WireSkills[1] != filepath.Join(home, "b") {
		t.Errorf("tilde not expanded: %q", c.WireSkills[1])
	}
}

func TestParseConfigAllowUserMergesWithFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("allowed_users = [1, 2]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--allow-user", "3"})
	if err != nil {
		t.Fatal(err)
	}
	// --allow-user is additive: file [1,2] + flag [3].
	if len(c.AllowedUsers) != 3 || c.AllowedUsers[2] != 3 {
		t.Errorf("expected merged [1 2 3], got %v", c.AllowedUsers)
	}
}
