package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestParseConfigAllowDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("allow_domains = [\"api.github.com\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--allow-domain", "*.githubusercontent.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Additive: file [api.github.com] + flag [*.githubusercontent.com]. Domains, not
	// paths — no ~/absolute munging (a leading *. must survive verbatim).
	if len(c.AllowDomains) != 2 || c.AllowDomains[0] != "api.github.com" || c.AllowDomains[1] != "*.githubusercontent.com" {
		t.Errorf("AllowDomains merge wrong: %v", c.AllowDomains)
	}
}

func TestParseConfigUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	body := "upload_command = \"/opt/up.sh\"\nupload_threshold_mb = 30\nupload_max_mb = 250\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The upload knobs are single-valued: --upload-max-mb overrides the file's 250.
	c, err := parseConfig([]string{"--config", path, "--upload-max-mb", "300"})
	if err != nil {
		t.Fatal(err)
	}
	if c.UploadCommand != "/opt/up.sh" {
		t.Errorf("UploadCommand = %q", c.UploadCommand)
	}
	if c.UploadThresholdMB != 30 {
		t.Errorf("UploadThresholdMB = %d, want 30", c.UploadThresholdMB)
	}
	if c.UploadMaxMB != 300 {
		t.Errorf("UploadMaxMB = %d, want 300 (flag overrides file)", c.UploadMaxMB)
	}
}

func TestParseConfigUploadDefaultThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("upload_command = \"/opt/up.sh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	// Threshold defaults to 40 only when the fallback is enabled.
	if c.UploadThresholdMB != 40 {
		t.Errorf("default threshold = %d, want 40", c.UploadThresholdMB)
	}
}

func TestValidateUploadCommandMissing(t *testing.T) {
	c := &Config{BotToken: "x", Profile: ProfileQA, Responder: ResponderClaude, Project: "/p",
		MaxConcurrent: 1, MaxIncomingMB: 20, OutboxTTL: "2h", UploadCommand: "/no/such/uploader", UploadThresholdMB: 40}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "upload_command") {
		t.Fatalf("want upload_command existence error, got %v", err)
	}
}

func TestValidateUploadMaxBelowThreshold(t *testing.T) {
	script := writeScript(t, `echo x`) // exists, so the stat check passes
	c := &Config{BotToken: "x", Profile: ProfileQA, Responder: ResponderClaude, Project: "/p",
		MaxConcurrent: 1, MaxIncomingMB: 20, OutboxTTL: "2h", UploadCommand: script, UploadThresholdMB: 40, UploadMaxMB: 30}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "upload_max_mb") {
		t.Fatalf("want upload_max_mb < threshold error, got %v", err)
	}
}

func TestParseConfigClaudeArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("claude_args = [\"--model\", \"opus\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Additive: file [--model opus] + flag [--effort high]; safe flags pass.
	c, err := parseConfig([]string{"--config", path, "--claude-arg", "--effort", "--claude-arg", "high"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ClaudeArgs, " "); got != "--model opus --effort high" {
		t.Errorf("ClaudeArgs merge wrong: %q", got)
	}

	// --claude-args is one whitespace-split string; combines with --claude-arg and
	// the file list, all additive (file, then --claude-arg, then --claude-args).
	c2, err := parseConfig([]string{"--config", path, "--claude-arg", "--verbose", "--claude-args", "  --effort   high "})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c2.ClaudeArgs, " "); got != "--model opus --verbose --effort high" {
		t.Errorf("--claude-args split/merge wrong: %q", got)
	}
}

func TestParseConfigClaudeArgsGuard(t *testing.T) {
	// Each ak-tgclaude-owned flag is rejected, in both `--flag value` and
	// `--flag=value` forms, so a passthrough can't silently override the sandbox,
	// transport, session, or output parsing.
	for _, bad := range []string{
		"--permission-mode", "--setting-sources", "--mcp-config", "--strict-mcp-config",
		"--allowedTools", "--settings", "--output-format", "--input-format",
		"--agent", "--resume", "-r", "--continue", "-c", "-p", "--print",
		"--dangerously-skip-permissions",
	} {
		if _, err := parseConfig([]string{"--claude-arg", bad, "--claude-arg", "x"}); err == nil {
			t.Errorf("claude-arg %q should be rejected", bad)
		}
		if _, err := parseConfig([]string{"--claude-arg", bad + "=x"}); err == nil {
			t.Errorf("claude-arg %q (=form) should be rejected", bad)
		}
	}
	// A safe flag with a value that itself looks flag-ish stays allowed.
	if _, err := parseConfig([]string{"--claude-arg", "--model", "--claude-arg", "opus"}); err != nil {
		t.Errorf("--model opus should be allowed: %v", err)
	}
}

func TestParseConfigPolicy(t *testing.T) {
	// Default is normal.
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string(c.Policy); len(got) != 1 || got[0] != "normal" {
		t.Errorf("default policy = %v, want [normal]", got)
	}
	// A built-in name is accepted.
	if c, err := parseConfig([]string{"--policy", "introspect"}); err != nil || len(c.Policy) != 1 || c.Policy[0] != "introspect" {
		t.Errorf("policy introspect: err=%v policy=%v", err, c.Policy)
	}
	// --policy is repeatable: entries accumulate in order.
	if c, err := parseConfig([]string{"--policy", "norefuse", "--policy", "introspect"}); err != nil ||
		len(c.Policy) != 2 || c.Policy[0] != "norefuse" || c.Policy[1] != "introspect" {
		t.Errorf("repeatable policy: err=%v policy=%v, want [norefuse introspect]", err, c.Policy)
	}
	// An unknown built-in name is rejected at startup — including when mixed with a
	// valid one.
	if _, err := parseConfig([]string{"--policy", "bogus"}); err == nil {
		t.Errorf("unknown policy name should be rejected")
	}
	if _, err := parseConfig([]string{"--policy", "normal", "--policy", "bogus"}); err == nil {
		t.Errorf("unknown policy name should be rejected even alongside a valid one")
	}
	// A path form must exist: a missing file is rejected.
	if _, err := parseConfig([]string{"--policy", "/no/such/policy.md"}); err == nil {
		t.Errorf("missing policy file should be rejected")
	}
	// An existing .md path is accepted (and kept as the resolved absolute path).
	f := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(f, []byte("persona"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--policy", f})
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Policy) != 1 || c2.Policy[0] != f {
		t.Errorf("policy path = %v, want [%q]", c2.Policy, f)
	}
}

func TestParseConfigPolicyTOML(t *testing.T) {
	// A bare string in TOML (the pre-list form) still decodes to a one-element list.
	strPath := filepath.Join(t.TempDir(), "str.toml")
	if err := os.WriteFile(strPath, []byte("policy = \"introspect\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", strPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policy) != 1 || c.Policy[0] != "introspect" {
		t.Errorf("string TOML policy = %v, want [introspect]", c.Policy)
	}
	// An array in TOML decodes in order; --policy is additive on top of it.
	arrPath := filepath.Join(t.TempDir(), "arr.toml")
	if err := os.WriteFile(arrPath, []byte("policy = [\"normal\", \"introspect\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--config", arrPath, "--policy", "norefuse"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Policy) != 3 || c2.Policy[0] != "normal" || c2.Policy[1] != "introspect" || c2.Policy[2] != "norefuse" {
		t.Errorf("array TOML + flag policy = %v, want [normal introspect norefuse]", c2.Policy)
	}
}

func TestParseConfigDeliveryGuard(t *testing.T) {
	// Default: guard ON (AllowSilent false), no fallback text.
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowSilent {
		t.Errorf("delivery guard should be ON by default (AllowSilent=false)")
	}
	// --allow-silent disables the guard.
	if c, err := parseConfig([]string{"--allow-silent"}); err != nil || !c.AllowSilent {
		t.Errorf("--allow-silent: err=%v AllowSilent=%v", err, c.AllowSilent)
	}
	// allow_silent + undelivered_text decode from TOML.
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("allow_silent = true\nundelivered_text = \"could not answer\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if !c2.AllowSilent || c2.UndeliveredText != "could not answer" {
		t.Errorf("TOML delivery config wrong: AllowSilent=%v UndeliveredText=%q", c2.AllowSilent, c2.UndeliveredText)
	}
}

func TestParseConfigTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("tools = [\"Agent\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", path, "--tool", "WebFetch", "--tool", "mcp__x__y"})
	if err != nil {
		t.Fatal(err)
	}
	// Additive: file [Agent] + flags [WebFetch, mcp__x__y], in order.
	if len(c.Tools) != 3 || c.Tools[0] != "Agent" || c.Tools[1] != "WebFetch" || c.Tools[2] != "mcp__x__y" {
		t.Errorf("tools merge wrong: %v", c.Tools)
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
