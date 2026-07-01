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
	// Sandboxed Bash reads of a sibling outbox are still masked (Bash isn't
	// hook-scoped); own outbox is carved back per invocation.
	if got := s.Sandbox.Filesystem.DenyRead; len(got) != 1 || got[0] != "/run/out" {
		t.Errorf("sandbox denyRead should be the outbox root, got %v", got)
	}
	if len(s.Sandbox.Credentials.Files) != 1 || s.Sandbox.Credentials.Files[0].Path != "/cfg/bot.toml" ||
		s.Sandbox.Credentials.Files[0].Mode != "deny" {
		t.Errorf("credentials.files = %+v", s.Sandbox.Credentials.Files)
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

func TestBuildSettingsNoTokenFile(t *testing.T) {
	s := buildSettings(scaffoldParams{CacheDir: "/c"})
	if s.Sandbox.Credentials.Files != nil {
		t.Errorf("no token file => no credentials.files, got %+v", s.Sandbox.Credentials.Files)
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
