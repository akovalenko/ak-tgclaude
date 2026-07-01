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
		CacheDir:  "/state/cache",
		TokenFile: "/cfg/bot.toml",
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
	for _, a := range s.Permissions.Allow {
		if strings.HasPrefix(a, "Write(") {
			t.Errorf("static settings must not grant Write, got %v", s.Permissions.Allow)
		}
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
