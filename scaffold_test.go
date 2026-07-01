package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSettingsShape(t *testing.T) {
	s := buildSettings(scaffoldParams{
		CacheDir:   "/state/cache",
		OutboxRoot: "/run/out",
		TokenFile:  "/cfg/bot.toml",
	})

	if !s.Sandbox.Enabled || !s.Sandbox.AutoAllowBashIfSandboxed || s.Sandbox.AllowUnsandboxedCommands {
		t.Errorf("sandbox flags wrong: %+v", s.Sandbox)
	}
	if s.Env["GOMODCACHE"] != "/state/cache/go-mod" {
		t.Errorf("GOMODCACHE = %q", s.Env["GOMODCACHE"])
	}
	// The static settings grant NO outbox write (that is per-invocation): only
	// the cache is writable, and only Read is allowed.
	if got := s.Sandbox.Filesystem.AllowWrite; len(got) != 1 || got[0] != "/state/cache" {
		t.Errorf("allowWrite should be just the cache, got %v", got)
	}
	// File tools are governed by the PreToolUse hook, so the static permissions
	// grant only the deferred tools (no Read/Write) and deny nothing.
	for _, a := range s.Permissions.Allow {
		if strings.HasPrefix(a, "Write(") || a == "Read" {
			t.Errorf("static settings must not grant Read/Write (the hook does), got %v", s.Permissions.Allow)
		}
	}
	if len(s.Permissions.Deny) != 0 {
		t.Errorf("static settings should carry no permissions.deny, got %v", s.Permissions.Deny)
	}
	// denyRead masks host history + other sessions' transcripts, plus the
	// sibling-outbox root (Bash isn't hook-scoped); own outbox is carved back per
	// invocation.
	if got := s.Sandbox.Filesystem.DenyRead; len(got) != 3 ||
		got[0] != "~/.claude/history.jsonl" || got[1] != "~/.claude/projects" || got[2] != "/run/out" {
		t.Errorf("sandbox denyRead = %v, want [~/.claude/history.jsonl ~/.claude/projects /run/out]", got)
	}
	// credentials.files: the host secrets (SSH keys, Claude token) always, then
	// the bot's config file since the token lives there.
	files := s.Sandbox.Credentials.Files
	if len(files) != 3 || files[0].Path != "~/.ssh" ||
		files[1].Path != "~/.claude/.credentials.json" || files[2].Path != "/cfg/bot.toml" {
		t.Errorf("credentials.files = %+v", files)
	}
	for _, f := range files {
		if f.Mode != "deny" {
			t.Errorf("credentials.files entry not deny: %+v", f)
		}
	}
	if len(s.Sandbox.Credentials.EnvVars) != 2 || s.Sandbox.Credentials.EnvVars[0].Mode != "deny" {
		t.Errorf("credentials.envVars = %+v", s.Sandbox.Credentials.EnvVars)
	}

	// Hook command references the deny-read path (quoted) by bare binary name.
	cmd := s.Hooks.PreToolUse[0].Hooks[0].Command
	if !strings.HasPrefix(cmd, "ak-tgclaude hook pretooluse") || !strings.Contains(cmd, "--deny-read '/cfg/bot.toml'") {
		t.Errorf("hook command = %q", cmd)
	}
}

func TestBuildSettingsBangBug(t *testing.T) {
	// Off by default: no --bang-bug in the hook command.
	off := buildSettings(scaffoldParams{CacheDir: "/c"})
	if strings.Contains(off.Hooks.PreToolUse[0].Hooks[0].Command, "--bang-bug") {
		t.Errorf("default should not pass --bang-bug: %q", off.Hooks.PreToolUse[0].Hooks[0].Command)
	}
	// On: the flag is appended to the generated hook command.
	on := buildSettings(scaffoldParams{CacheDir: "/c", BangBug: true})
	if !strings.Contains(on.Hooks.PreToolUse[0].Hooks[0].Command, "--bang-bug") {
		t.Errorf("BangBug should pass --bang-bug: %q", on.Hooks.PreToolUse[0].Hooks[0].Command)
	}
}

func TestBuildSettingsDenyRead(t *testing.T) {
	// Operator --deny-read paths land at BOTH layers: sandbox.filesystem.denyRead
	// (the Bash `cat`/`grep` path) and the hook's --deny-read (the Read tool).
	s := buildSettings(scaffoldParams{
		CacheDir:   "/c",
		OutboxRoot: "/run/out",
		TokenFile:  "/cfg/bot.toml",
		DenyRead:   []string{"/secret/a", "~/b"},
	})

	// Bash layer: host secrets (2), then operator paths, then the outbox root.
	want := []string{"~/.claude/history.jsonl", "~/.claude/projects", "/secret/a", "~/b", "/run/out"}
	got := s.Sandbox.Filesystem.DenyRead
	if len(got) != len(want) {
		t.Fatalf("denyRead = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("denyRead[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}

	// Read-tool layer: the hook command carries each operator path (quoted) plus
	// the token file.
	cmd := s.Hooks.PreToolUse[0].Hooks[0].Command
	for _, p := range []string{"--deny-read '/secret/a'", "--deny-read '~/b'", "--deny-read '/cfg/bot.toml'"} {
		if !strings.Contains(cmd, p) {
			t.Errorf("hook command missing %q: %q", p, cmd)
		}
	}
}

func TestBuildSettingsNoTokenFile(t *testing.T) {
	s := buildSettings(scaffoldParams{CacheDir: "/c"})
	// Even without a bot token, the host secrets are always denied; only the bot
	// config file is absent.
	files := s.Sandbox.Credentials.Files
	if len(files) != 2 || files[0].Path != "~/.ssh" || files[1].Path != "~/.claude/.credentials.json" {
		t.Errorf("host secrets should always be denied (no token), got %+v", files)
	}
	if cmd := s.Hooks.PreToolUse[0].Hooks[0].Command; strings.Contains(cmd, "--deny-read") {
		t.Errorf("no token file => hook has no --deny-read, got %q", cmd)
	}
}

func TestMaterializeScaffoldWritesValidJSON(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", TokenFile: "/cfg/bot.toml"}); err != nil {
		t.Fatalf("materializeScaffold: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("generated settings.json is not valid JSON: %v", err)
	}
	if _, ok := round["hooks"]; !ok {
		t.Errorf("settings.json missing hooks")
	}

	// The embedded responder agent and emission skill must be materialized too.
	for _, rel := range []string{
		filepath.Join("agents", defaultAgent+".md"),
		filepath.Join("skills", "tg-emit", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(cwd, ".claude", rel)); err != nil {
			t.Errorf("asset not materialized: %s (%v)", rel, err)
		}
	}
}

func agentBody(t *testing.T, noRefuse bool) string {
	t.Helper()
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", NoRefuse: noRefuse}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMaterializeSubstitutesProjectRoot(t *testing.T) {
	dir := t.TempDir()
	// Placeholder replaced when a project is set.
	p := filepath.Join(dir, "a.md")
	if err := materializeFile(p, []byte("root={{PROJECT}}/x"), "/proj"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "root=/proj/x" {
		t.Errorf("substitution: got %q, want %q", b, "root=/proj/x")
	}
	// Empty project leaves the placeholder visible (not a broken empty path).
	q := filepath.Join(dir, "b.md")
	if err := materializeFile(q, []byte("root={{PROJECT}}/x"), ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(q); string(b) != "root={{PROJECT}}/x" {
		t.Errorf("empty project should not substitute: got %q", b)
	}
}

// writeSkill creates a skill template dir <root>/<name>/SKILL.md with body, and
// returns the skill dir path.
func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWireSkillMaterializesAndPreloads(t *testing.T) {
	skillDir := writeSkill(t, t.TempDir(), "eputs-qa-knowledge",
		"---\nname: eputs-qa-knowledge\ndescription: eputs domain\n---\nSources live under {{PROJECT}}/notes/eputs.\n")

	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{
		CacheDir:   "/c",
		Project:    "/home/me/thoughts",
		WireSkills: []string{skillDir},
	}); err != nil {
		t.Fatalf("materializeScaffold: %v", err)
	}

	// The wired skill is materialized with {{PROJECT}} substituted for the project.
	got, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "eputs-qa-knowledge", "SKILL.md"))
	if err != nil {
		t.Fatalf("wired skill not materialized: %v", err)
	}
	if strings.Contains(string(got), "{{PROJECT}}") {
		t.Errorf("placeholder not substituted:\n%s", got)
	}
	if !strings.Contains(string(got), "/home/me/thoughts/notes/eputs") {
		t.Errorf("project path not substituted in:\n%s", got)
	}

	// The built-in agent preloads it alongside the built-in tg-emit (order-preserving).
	agent, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "skills: [tg-emit, eputs-qa-knowledge]") {
		t.Errorf("agent skills: not wired as expected:\n%s", agent)
	}
}

func TestWireSkillAcceptsFileOrDir(t *testing.T) {
	src := t.TempDir()
	skillDir := writeSkill(t, src, "my-skill", "---\nname: my-skill\n---\nbody")
	skillFile := filepath.Join(skillDir, "SKILL.md")

	for _, path := range []string{skillDir, skillFile} {
		claudeDir := filepath.Join(t.TempDir(), ".claude")
		names, err := wireSkills(claudeDir, "/proj", []string{path})
		if err != nil {
			t.Fatalf("wireSkills(%s): %v", path, err)
		}
		if len(names) != 1 || names[0] != "my-skill" {
			t.Errorf("path %s: names = %v, want [my-skill]", path, names)
		}
		if _, err := os.Stat(filepath.Join(claudeDir, "skills", "my-skill", "SKILL.md")); err != nil {
			t.Errorf("path %s: skill not materialized: %v", path, err)
		}
	}
}

func TestWireSkillDedupsAndInsertsWhenAbsent(t *testing.T) {
	// Merging into an existing list de-duplicates.
	got := string(appendAgentSkills([]byte("---\nname: x\nskills: [tg-emit]\n---\nbody"), []string{"tg-emit", "d"}))
	if !strings.Contains(got, "skills: [tg-emit, d]") {
		t.Errorf("dedup/merge wrong:\n%s", got)
	}
	// A skills: line is inserted when the frontmatter has none.
	ins := string(appendAgentSkills([]byte("---\nname: x\n---\nbody"), []string{"d"}))
	if !strings.Contains(ins, "skills: [d]") {
		t.Errorf("insert wrong:\n%s", ins)
	}
	// No frontmatter => returned unchanged.
	raw := "no frontmatter here"
	if out := string(appendAgentSkills([]byte(raw), []string{"d"})); out != raw {
		t.Errorf("no-frontmatter should be unchanged: %q", out)
	}
}

func TestMaterializeAgentVariant(t *testing.T) {
	// Same agent NAME either way (so --agent selection is unchanged).
	def := agentBody(t, false)
	nr := agentBody(t, true)
	for _, body := range []string{def, nr} {
		if !strings.Contains(body, "name: "+defaultAgent) {
			t.Errorf("variant lost the agent name:\n%s", body)
		}
	}
	// Default declines off-topic; --norefuse does not.
	if !strings.Contains(def, "out of scope") {
		t.Errorf("default agent should mention scope: %q", def)
	}
	if !strings.Contains(nr, "Do NOT decline") {
		t.Errorf("norefuse agent should say not to decline: %q", nr)
	}
	if strings.Contains(nr, "untrusted input") {
		t.Errorf("norefuse agent should not carry the untrusted-input refusal framing")
	}
}
