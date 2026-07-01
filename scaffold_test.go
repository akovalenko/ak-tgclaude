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
	if got := s.Sandbox.Filesystem.AllowWrite; len(got) != 2 || got[0] != "/run/out" {
		t.Errorf("allowWrite = %v", got)
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

	// Write permission is confined to the outbox root.
	foundWrite := false
	for _, a := range s.Permissions.Allow {
		if a == "Write(/run/out/**)" {
			foundWrite = true
		}
	}
	if !foundWrite {
		t.Errorf("permissions.allow missing outbox write: %v", s.Permissions.Allow)
	}
}

func TestBuildSettingsNoTokenFile(t *testing.T) {
	s := buildSettings(scaffoldParams{CacheDir: "/c", OutboxRoot: "/o"})
	if s.Sandbox.Credentials.Files != nil {
		t.Errorf("no token file => no credentials.files, got %+v", s.Sandbox.Credentials.Files)
	}
	if cmd := s.Hooks.PreToolUse[0].Hooks[0].Command; strings.Contains(cmd, "--deny-read") {
		t.Errorf("no token file => hook has no --deny-read, got %q", cmd)
	}
}

func TestMaterializeScaffoldWritesValidJSON(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", OutboxRoot: "/o", TokenFile: "/cfg/bot.toml"}); err != nil {
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
}
