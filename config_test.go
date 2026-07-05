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

func TestParseConfigUsageLog(t *testing.T) {
	// Default: off (empty path).
	d, _ := parseConfig(nil)
	if d.UsageLog != "" {
		t.Errorf("usage_log should default off, got %q", d.UsageLog)
	}
	// --usage-log sets it, expanded to an absolute path like every other path field.
	c, err := parseConfig([]string{"--usage-log", "var/usage.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.UsageLog) || !strings.HasSuffix(c.UsageLog, string(os.PathSeparator)+filepath.Join("var", "usage.jsonl")) {
		t.Errorf("usage_log should resolve to an absolute path, got %q", c.UsageLog)
	}
	// File value works; CLI overrides it.
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("usage_log = \"/data/from-file.jsonl\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cf, err := parseConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cf.UsageLog != "/data/from-file.jsonl" {
		t.Errorf("file usage_log = %q, want /data/from-file.jsonl", cf.UsageLog)
	}
	cflag, err := parseConfig([]string{"--config", path, "--usage-log", "/data/from-flag.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if cflag.UsageLog != "/data/from-flag.jsonl" {
		t.Errorf("CLI usage_log should override file, got %q", cflag.UsageLog)
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

func TestParseConfigTranscriptsDefaultOff(t *testing.T) {
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	c.applyDefaults()
	if c.Transcripts {
		t.Error("transcripts should default off")
	}
	if c.TranscriptRoot() != "" {
		t.Errorf("root should be empty when off, got %q", c.TranscriptRoot())
	}
	if !c.OwnerReadsAllTranscripts() {
		t.Error("owner_reads_all should default true")
	}
	if c2, _ := parseConfig([]string{"--transcripts"}); !c2.Transcripts {
		t.Error("--transcripts should enable the feature")
	}
}

func TestConfigTranscriptRootDefault(t *testing.T) {
	c, _ := parseConfig([]string{"--transcripts"})
	c.StateDir = "/s"
	c.applyDefaults() // keeps the set StateDir
	if got := c.TranscriptRoot(); got != filepath.Join("/s", "transcripts") {
		t.Errorf("default root: got %q", got)
	}
	// workdir moves the store under <workdir>/state (beside the session store).
	cw, _ := parseConfig([]string{"--transcripts", "--workdir", "/w"})
	cw.applyDefaults()
	if got := cw.TranscriptRoot(); got != filepath.Join("/w", "state", "transcripts") {
		t.Errorf("workdir root: got %q", got)
	}
}

func TestConfigTranscriptDirOverride(t *testing.T) {
	c, err := parseConfig([]string{"--transcripts", "--transcript-dir", "/data/tr"})
	if err != nil {
		t.Fatal(err)
	}
	c.applyDefaults()
	if got := c.TranscriptRoot(); got != "/data/tr" {
		t.Errorf("override root: got %q", got)
	}
}

func TestParseConfigOwnerReadsAll(t *testing.T) {
	// Unset => default true.
	c, _ := parseConfig(nil)
	c.applyDefaults()
	if !c.OwnerReadsAllTranscripts() {
		t.Error("unset owner_reads_all should be true")
	}
	// CLI false wins.
	cf, _ := parseConfig([]string{"--owner-reads-all=false"})
	cf.applyDefaults()
	if cf.OwnerReadsAllTranscripts() {
		t.Error("--owner-reads-all=false should be false")
	}
	// File false wins when no flag is passed.
	path := filepath.Join(t.TempDir(), "bot.toml")
	if err := os.WriteFile(path, []byte("owner_reads_all = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cff, _ := parseConfig([]string{"--config", path})
	cff.applyDefaults()
	if cff.OwnerReadsAllTranscripts() {
		t.Error("file owner_reads_all=false should win when no flag")
	}
	// CLI true overrides file false (fs.Visit only fires for a passed flag).
	cft, _ := parseConfig([]string{"--config", path, "--owner-reads-all=true"})
	cft.applyDefaults()
	if !cft.OwnerReadsAllTranscripts() {
		t.Error("--owner-reads-all=true should override file false")
	}
}

func TestValidateTranscriptDirUnderProject(t *testing.T) {
	c := &Config{BotToken: "x", Profile: ProfileQA, Responder: ResponderClaude, Project: "/proj",
		MaxConcurrent: 1, MaxIncomingMB: 20, OutboxTTL: "2h", Transcripts: true, TranscriptDir: "/proj/tr"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "transcript_dir") {
		t.Fatalf("want transcript_dir-under-project error, got %v", err)
	}
	c.TranscriptDir = "/data/tr" // elsewhere: fine
	if err := c.validate(); err != nil {
		t.Fatalf("safe transcript_dir should validate, got %v", err)
	}
}

func TestParseConfigTranscriptDirGlobRejected(t *testing.T) {
	if _, err := parseConfig([]string{"--transcripts", "--transcript-dir", "/data/tr*"}); err == nil {
		t.Error("glob metachar in transcript_dir should be rejected")
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
	if got := []string(c.Policies); len(got) != 1 || got[0] != "normal" {
		t.Errorf("default policies = %v, want [normal]", got)
	}
	// A built-in name is accepted. introspect is axis-less, so the refusal-axis floor
	// prepends normal as the base stance: [normal introspect].
	if c, err := parseConfig([]string{"--policy", "introspect"}); err != nil ||
		len(c.Policies) != 2 || c.Policies[0] != "normal" || c.Policies[1] != "introspect" {
		t.Errorf("policy introspect: err=%v policies=%v, want [normal introspect]", err, c.Policies)
	}
	// strict is a built-in (the new hard refusal stance).
	if _, err := parseConfig([]string{"--policy", "strict"}); err != nil {
		t.Errorf("strict should be a built-in policy: %v", err)
	}
	// outbox-rw is a built-in and, being axis-less, stacks on a refusal stance.
	if c, err := parseConfig([]string{"--policy", "strict", "--policy", "outbox-rw"}); err != nil ||
		len(c.Policies) != 2 || c.Policies[0] != "strict" || c.Policies[1] != "outbox-rw" {
		t.Errorf("strict + outbox-rw: err=%v policies=%v, want [strict outbox-rw]", err, c.Policies)
	}
	// --policy is repeatable: entries accumulate in order (norefuse is refusal-axis,
	// introspect is axis-less, so they don't conflict).
	if c, err := parseConfig([]string{"--policy", "norefuse", "--policy", "introspect"}); err != nil ||
		len(c.Policies) != 2 || c.Policies[0] != "norefuse" || c.Policies[1] != "introspect" {
		t.Errorf("repeatable policy: err=%v policies=%v, want [norefuse introspect]", err, c.Policies)
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
	// An existing .md path is accepted (and kept as the resolved absolute path). It is
	// axis-less, so the floor prepends normal as the base: [normal <path>].
	f := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(f, []byte("persona"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--policy", f})
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Policies) != 2 || c2.Policies[0] != "normal" || c2.Policies[1] != f {
		t.Errorf("policy path = %v, want [normal %q]", c2.Policies, f)
	}
	// Escape hatch: a custom fragment that itself declares `axis: refusal` occupies the
	// slot, so the floor does NOT prepend normal — the persona is deliberately base-less.
	fr := filepath.Join(t.TempDir(), "refusal.md")
	if err := os.WriteFile(fr, []byte("---\naxis: refusal\n---\nYou are my own base stance.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c3, err := parseConfig([]string{"--policy", fr})
	if err != nil {
		t.Fatal(err)
	}
	if len(c3.Policies) != 1 || c3.Policies[0] != fr {
		t.Errorf("axis:refusal custom fragment = %v, want [%q] (no normal floor)", c3.Policies, fr)
	}
}

// Two default fragments on the same axis (normal/norefuse/strict all carry
// axis: refusal) are a load-time error — the opt-in mutual-exclusion guard.
func TestParseConfigPolicyAxisConflict(t *testing.T) {
	if _, err := parseConfig([]string{"--policy", "normal", "--policy", "norefuse"}); err == nil {
		t.Errorf("normal + norefuse share axis refusal — should be rejected")
	}
	if _, err := parseConfig([]string{"--policy", "strict", "--policy", "norefuse"}); err == nil {
		t.Errorf("strict + norefuse share axis refusal — should be rejected")
	}
	// An axis-less fragment (introspect) alongside a refusal one is fine.
	if _, err := parseConfig([]string{"--policy", "strict", "--policy", "introspect"}); err != nil {
		t.Errorf("strict + introspect should be allowed: %v", err)
	}
}

func TestParseConfigPolicyTOML(t *testing.T) {
	// A bare string in TOML still decodes to a one-element list. introspect is
	// axis-less, so the refusal-axis floor prepends normal: [normal introspect].
	strPath := filepath.Join(t.TempDir(), "str.toml")
	if err := os.WriteFile(strPath, []byte("policies = \"introspect\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", strPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policies) != 2 || c.Policies[0] != "normal" || c.Policies[1] != "introspect" {
		t.Errorf("string TOML policies = %v, want [normal introspect]", c.Policies)
	}
	// An array in TOML decodes in order; --policy is additive on top of it.
	arrPath := filepath.Join(t.TempDir(), "arr.toml")
	if err := os.WriteFile(arrPath, []byte("policies = [\"norefuse\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--config", arrPath, "--policy", "introspect"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Policies) != 2 || c2.Policies[0] != "norefuse" || c2.Policies[1] != "introspect" {
		t.Errorf("array TOML + flag policies = %v, want [norefuse introspect]", c2.Policies)
	}
	// The old singular `policy` key is a hard error (renamed to `policies`).
	legacyPath := filepath.Join(t.TempDir(), "legacy.toml")
	if err := os.WriteFile(legacyPath, []byte("policy = \"norefuse\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig([]string{"--config", legacyPath}); err == nil {
		t.Errorf("legacy `policy` key should be rejected")
	}
}

// Per-user overrides layer on the default along axes: an override fragment evicts
// the default fragment on its axis, an axis-less one appends. PersonaSelectors
// exposes the resolved list.
func TestParseConfigPolicyOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ov.toml")
	body := "policies = [\"strict\"]\n\n[policy_overrides]\n123 = [\"norefuse\", \"introspect\"]\n456 = [\"introspect\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", p})
	if err != nil {
		t.Fatal(err)
	}
	// 123: norefuse evicts strict (shared refusal axis), introspect appended.
	if got := c.PersonaSelectors(123); len(got) != 2 || got[0] != "norefuse" || got[1] != "introspect" {
		t.Errorf("override 123 = %v, want [norefuse introspect]", got)
	}
	// 456: introspect is axis-less, so strict stays and introspect appends.
	if got := c.PersonaSelectors(456); len(got) != 2 || got[0] != "strict" || got[1] != "introspect" {
		t.Errorf("override 456 = %v, want [strict introspect]", got)
	}
	// An unknown user gets the default.
	if got := c.PersonaSelectors(999); len(got) != 1 || got[0] != "strict" {
		t.Errorf("default persona = %v, want [strict]", got)
	}
}

// A non-numeric override key, and an override list that itself conflicts on an
// axis, both fail at load.
func TestParseConfigPolicyOverridesInvalid(t *testing.T) {
	badKey := filepath.Join(t.TempDir(), "badkey.toml")
	if err := os.WriteFile(badKey, []byte("[policy_overrides]\nalice = [\"norefuse\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig([]string{"--config", badKey}); err == nil {
		t.Errorf("non-numeric override key should be rejected")
	}
	badAxis := filepath.Join(t.TempDir(), "badaxis.toml")
	if err := os.WriteFile(badAxis, []byte("[policy_overrides]\n7 = [\"normal\", \"norefuse\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig([]string{"--config", badAxis}); err == nil {
		t.Errorf("override with two refusal fragments should be rejected")
	}
}

// owner is auto-whitelisted and granted the relaxed owner persona, unless it has an
// explicit override entry (which then wins).
func TestParseConfigOwner(t *testing.T) {
	c, err := parseConfig([]string{"--owner", "555"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsInt64(c.AllowedUsers, 555) {
		t.Errorf("owner should be auto-whitelisted: allowed=%v", c.AllowedUsers)
	}
	// Default policies are [normal]; the owner bundle (norefuse+introspect) evicts
	// normal on the refusal axis and appends introspect.
	if got := c.PersonaSelectors(555); len(got) != 2 || got[0] != "norefuse" || got[1] != "introspect" {
		t.Errorf("owner persona = %v, want [norefuse introspect]", got)
	}
	// An explicit override for the owner wins over the default owner bundle.
	p := filepath.Join(t.TempDir(), "owner.toml")
	if err := os.WriteFile(p, []byte("owner = 555\npolicies = [\"strict\"]\n\n[policy_overrides]\n555 = [\"introspect\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfig([]string{"--config", p})
	if err != nil {
		t.Fatal(err)
	}
	// strict stays (introspect is axis-less); NOT the owner norefuse bundle.
	if got := c2.PersonaSelectors(555); len(got) != 2 || got[0] != "strict" || got[1] != "introspect" {
		t.Errorf("explicit owner override = %v, want [strict introspect]", got)
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
