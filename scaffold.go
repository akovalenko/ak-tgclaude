package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The responder's generated .claude/settings.json is built from these structs
// (not a text template) so the literal, runtime-computed paths are inserted
// safely. It is modeled on the live murphy tgbot settings: a sandboxed,
// auto-allowed Bash environment with isolated Go caches, plus this project's
// token guard (PreToolUse hook + credentials deny-read).

type claudeSettings struct {
	Env         map[string]string `json:"env,omitempty"`
	Permissions *permissionsCfg   `json:"permissions,omitempty"`
	Sandbox     *sandboxCfg       `json:"sandbox,omitempty"`
	Hooks       *hooksCfg         `json:"hooks,omitempty"`
}

type permissionsCfg struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type sandboxCfg struct {
	Enabled                  bool            `json:"enabled"`
	AutoAllowBashIfSandboxed bool            `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool            `json:"allowUnsandboxedCommands"`
	Network                  *networkCfg     `json:"network,omitempty"`
	Filesystem               *filesystemCfg  `json:"filesystem,omitempty"`
	Credentials              *credentialsCfg `json:"credentials,omitempty"`
}

type networkCfg struct {
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

type filesystemCfg struct {
	AllowWrite []string `json:"allowWrite,omitempty"`
}

type credentialsCfg struct {
	Files   []credFile `json:"files,omitempty"`
	EnvVars []credEnv  `json:"envVars,omitempty"`
}

type credFile struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

type credEnv struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type hooksCfg struct {
	PreToolUse []hookMatcher `json:"PreToolUse,omitempty"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

// scaffoldParams are the runtime-computed values baked into settings.json.
type scaffoldParams struct {
	CacheDir       string   // isolated Go caches root
	OutboxRoot     string   // writable root that holds per-invocation outboxes
	TokenFile      string   // config file holding the token; "" if token came via --bot-token
	HookBinary     string   // default "ak-tgclaude"
	DenyEnvVars    []string // secrets to unset in the sandbox
	NetworkDomains []string // sandbox egress allowlist
}

// defaultDenyEnvVars are the ambient secrets scrubbed from the responder's
// sandboxed shell (its own model calls resolve the key before this bites).
var defaultDenyEnvVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// defaultNetworkDomains is the egress the responder needs to build Go code.
var defaultNetworkDomains = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}

// buildSettings assembles the responder's .claude/settings.json.
func buildSettings(p scaffoldParams) *claudeSettings {
	if p.HookBinary == "" {
		p.HookBinary = "ak-tgclaude"
	}
	if len(p.DenyEnvVars) == 0 {
		p.DenyEnvVars = defaultDenyEnvVars
	}
	if len(p.NetworkDomains) == 0 {
		p.NetworkDomains = defaultNetworkDomains
	}

	envVars := make([]credEnv, 0, len(p.DenyEnvVars))
	for _, name := range p.DenyEnvVars {
		envVars = append(envVars, credEnv{Name: name, Mode: "deny"})
	}

	s := &claudeSettings{
		Env: map[string]string{
			"GOCACHE":             filepath.Join(p.CacheDir, "go-build"),
			"GOMODCACHE":          filepath.Join(p.CacheDir, "go-mod"),
			"GOLANGCI_LINT_CACHE": filepath.Join(p.CacheDir, "golangci-lint"),
			"GOPATH":              filepath.Join(p.CacheDir, "gopath"),
		},
		Permissions: &permissionsCfg{
			// Read is broad (the token stays protected by the hook + deny-read
			// below regardless); Write is confined to the outbox root, where the
			// responder drops its message body files and `send` descriptors.
			Allow: []string{"Read", "Write(" + p.OutboxRoot + "/**)"},
		},
		Sandbox: &sandboxCfg{
			Enabled:                  true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			Network:                  &networkCfg{AllowedDomains: p.NetworkDomains},
			Filesystem:               &filesystemCfg{AllowWrite: []string{p.OutboxRoot, p.CacheDir}},
			Credentials:              &credentialsCfg{EnvVars: envVars},
		},
	}
	if p.TokenFile != "" {
		s.Sandbox.Credentials.Files = []credFile{{Path: p.TokenFile, Mode: "deny"}}
	}

	hookCmd := p.HookBinary + " hook pretooluse"
	if p.TokenFile != "" {
		hookCmd += " --deny-read " + shellQuote(p.TokenFile)
	}
	s.Hooks = &hooksCfg{PreToolUse: []hookMatcher{{
		Matcher: "*",
		Hooks: []hookEntry{{
			Type:          "command",
			Command:       hookCmd,
			Timeout:       10,
			StatusMessage: "ak-tgclaude token guard",
		}},
	}}}
	return s
}

// materializeScaffold writes the generated settings.json into <cwd>/.claude.
// The cwd is the responder's launch dir (used with `claude -p --setting-sources
// project`); the binary embeds no finished settings — it is generated here with
// the literal runtime paths.
func materializeScaffold(cwd string, p scaffoldParams) error {
	claudeDir := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", claudeDir, err)
	}
	b, err := json.MarshalIndent(buildSettings(p), "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// shellQuote single-quotes a path for embedding in the hook command string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
