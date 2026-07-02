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

func TestParseConfigEphemeralAndBillFlags(t *testing.T) {
	c, err := parseConfig([]string{"--ephemeral-sessions", "--bill", "--bang-bug"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.EphemeralSessions {
		t.Error("--ephemeral-sessions should set EphemeralSessions")
	}
	if !c.Bill {
		t.Error("--bill should set Bill")
	}
	if !c.BangBug {
		t.Error("--bang-bug should set BangBug")
	}
	// All default off when unset.
	d, _ := parseConfig(nil)
	if d.EphemeralSessions || d.Bill || d.BangBug {
		t.Errorf("defaults should be off: ephemeral=%v bill=%v bang=%v", d.EphemeralSessions, d.Bill, d.BangBug)
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

func TestParseConfigAddSkillsAndAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("add_skills = [\"/lib/s\"]\nadd_agents = [\"/lib/a.md\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--add-skill", "/lib/s2", "--add-agent", "/lib/a2.md"})
	if err != nil {
		t.Fatal(err)
	}
	// Both are additive (file list + repeatable flags), like wire_skills.
	if len(c.AddSkills) != 2 || c.AddSkills[0] != "/lib/s" || c.AddSkills[1] != "/lib/s2" {
		t.Errorf("AddSkills merge wrong: %v", c.AddSkills)
	}
	if len(c.AddAgents) != 2 || c.AddAgents[0] != "/lib/a.md" || c.AddAgents[1] != "/lib/a2.md" {
		t.Errorf("AddAgents merge wrong: %v", c.AddAgents)
	}
}

func TestParseConfigDenyRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("deny_reads = [\"/etc/secret\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--deny-read", "~/priv", "--deny-read", "/var/token"})
	if err != nil {
		t.Fatal(err)
	}
	// Additive: file [/etc/secret] + flags [~/priv, /var/token].
	if len(c.DenyRead) != 3 {
		t.Fatalf("expected 3 deny-read paths, got %v", c.DenyRead)
	}
	if c.DenyRead[0] != "/etc/secret" || c.DenyRead[2] != "/var/token" {
		t.Errorf("deny-read order/merge wrong: %v", c.DenyRead)
	}
	// A leading ~ is expanded (like project/wire_skills), so the hook's absolute
	// path match works.
	home, _ := os.UserHomeDir()
	if c.DenyRead[1] != filepath.Join(home, "priv") {
		t.Errorf("tilde not expanded: %q", c.DenyRead[1])
	}
}

func TestParseConfigDenyEnvs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("deny_envs = [\"FOO\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--deny-env", "BAR"})
	if err != nil {
		t.Fatal(err)
	}
	// Additive: file [FOO] + flag [BAR]. Names, not paths — no ~/absolute munging.
	if len(c.DenyEnvs) != 2 || c.DenyEnvs[0] != "FOO" || c.DenyEnvs[1] != "BAR" {
		t.Errorf("DenyEnvs merge wrong: %v", c.DenyEnvs)
	}
}

func TestParseConfigResolvesRelativePaths(t *testing.T) {
	// Every path field is absolutized against the launch cwd, so it is
	// unambiguous once the responder consumes it from the scaffold cwd.
	c, err := parseConfig([]string{
		"--project", "code/proj",
		"--wire-skill", "lib/skill",
		"--deny-read", "sec/env",
	})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if want := filepath.Join(wd, "code/proj"); c.Project != want {
		t.Errorf("Project = %q, want %q", c.Project, want)
	}
	if want := filepath.Join(wd, "lib/skill"); len(c.WireSkills) != 1 || c.WireSkills[0] != want {
		t.Errorf("WireSkills = %v, want [%q]", c.WireSkills, want)
	}
	if want := filepath.Join(wd, "sec/env"); len(c.DenyRead) != 1 || c.DenyRead[0] != want {
		t.Errorf("DenyRead = %v, want [%q]", c.DenyRead, want)
	}
}

func TestParseConfigRejectsGlobPaths(t *testing.T) {
	// A glob metacharacter (or control char) in any path field is rejected up
	// front: the sandbox filesystem rules glob-match, so it would silently protect
	// the wrong files. Covers project, wire_skills, deny_reads.
	cases := [][]string{
		{"--project", "/code/*"},
		{"--project", "/a[b]/c"},
		{"--deny-read", "/secrets/*.env"},
		{"--deny-read", "/x?y"},
		{"--deny-read", `/back\slash`},
		{"--wire-skill", "/lib/skill[1]"},
		{"--project", "/code/\tnasty"},
	}
	for _, args := range cases {
		if _, err := parseConfig(args); err == nil {
			t.Errorf("parseConfig(%v) should reject an exotic path", args)
		}
	}
}

func TestParseConfigAllowsSpacesAndQuotes(t *testing.T) {
	// Spaces and single quotes are NOT glob metacharacters — shellQuote handles the
	// hook command and fnmatch treats them literally, so they must be accepted (a
	// path like /Users/o'brien/My Project is legitimate).
	c, err := parseConfig([]string{
		"--project", "/home/o'brien/my project",
		"--deny-read", "/vault/api key.txt",
	})
	if err != nil {
		t.Fatalf("legit path with space/quote rejected: %v", err)
	}
	if c.Project != "/home/o'brien/my project" {
		t.Errorf("project mangled: %q", c.Project)
	}
	if len(c.DenyRead) != 1 || c.DenyRead[0] != "/vault/api key.txt" {
		t.Errorf("deny-read mangled: %v", c.DenyRead)
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
