package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// scaffoldAssets are the static responder assets copied into <cwd>/.claude:
// the responder agent(s) and the shared emission skill. They carry no runtime
// paths, so they are embedded verbatim (unlike settings.json, which is
// generated).
//
//go:embed assets
var scaffoldAssets embed.FS

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
// Note: the outbox is NOT granted here — write access to a specific outbox is
// added per invocation via buildInvocationSettings, so concurrent responders are
// each confined to their own.
type scaffoldParams struct {
	CacheDir       string   // isolated Go caches root
	TokenFile      string   // config file holding the token; "" if token came via --bot-token
	HookBinary     string   // default "ak-tgclaude"
	DenyEnvVars    []string // secrets to unset in the sandbox
	NetworkDomains []string // sandbox egress allowlist
	NoRefuse       bool     // materialize the do-what-you're-asked agent variant
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
			// below regardless). Write is NOT granted here: each invocation gets
			// write access to exactly its own outbox via a per-invocation
			// --settings overlay (buildInvocationSettings), so concurrent
			// responders cannot write into each other's outbox.
			Allow: []string{"Read"},
		},
		Sandbox: &sandboxCfg{
			Enabled:                  true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			Network:                  &networkCfg{AllowedDomains: p.NetworkDomains},
			Filesystem:               &filesystemCfg{AllowWrite: []string{p.CacheDir}},
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
	if err := materializeSkills(claudeDir); err != nil {
		return err
	}
	return materializeAgent(claudeDir, p.NoRefuse)
}

// materializeSkills copies the embedded skills tree into <cwd>/.claude/skills.
func materializeSkills(claudeDir string) error {
	return fs.WalkDir(scaffoldAssets, "assets/skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("assets", p)
		if err != nil {
			return err
		}
		dst := filepath.Join(claudeDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := scaffoldAssets.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
}

// materializeAgent writes the chosen responder agent variant into
// <cwd>/.claude/agents/faq-responder.md. Both variants carry the same agent name
// (so --agent selection is unchanged); noRefuse swaps the persona from a scoped
// FAQ that declines off-topic to a do-what-you're-asked assistant. Machine
// guards (sandbox, token deny-read, per-invocation write, pinned route) hold
// either way, so the relaxed persona cannot exceed them.
func materializeAgent(claudeDir string, noRefuse bool) error {
	src := "assets/agents/faq-responder.md"
	if noRefuse {
		src = "assets/agents/faq-responder.norefuse.md"
	}
	data, err := scaffoldAssets.ReadFile(src)
	if err != nil {
		return err
	}
	dir := filepath.Join(claudeDir, "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "faq-responder.md"), data, 0o600)
}

// buildInvocationSettings returns the per-invocation --settings JSON that grants
// write access to exactly one outbox — both the Write tool (permissions.allow)
// and sandboxed Bash (sandbox.filesystem.allowWrite) — merged on top of the
// static project settings. This confines each concurrent responder to its own
// outbox: it can neither Write-tool nor Bash-write (cp / `send --outbox`) into
// another chat's. Empty outbox => "".
func buildInvocationSettings(outbox string) string {
	if outbox == "" {
		return ""
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	s.Permissions.Allow = []string{"Write(" + outbox + "/**)"}
	s.Sandbox.Filesystem.AllowWrite = []string{outbox}
	b, _ := json.Marshal(&s)
	return string(b)
}

// shellQuote single-quotes a path for embedding in the hook command string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runScaffold materializes the responder cwd WITHOUT running the dispatcher, so
// the operator can inspect the generated settings, tweak settings.local.json,
// and run `claude` there by hand to observe the sandbox. Everything is
// self-contained under --cwd (settings + outbox), so it can be blown away.
func runScaffold(args []string) {
	cfg, err := parseConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(2)
	}
	if cfg.Cwd == "" {
		fmt.Fprintln(os.Stderr, "ak-tgclaude: scaffold: --cwd is required (dir to materialize into)")
		os.Exit(2)
	}
	if err := os.MkdirAll(cfg.Cwd, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}
	outboxRoot := filepath.Join(cfg.Cwd, "outbox")
	if err := os.MkdirAll(outboxRoot, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}
	if err := materializeScaffold(cfg.Cwd, scaffoldParams{
		CacheDir:  filepath.Join(cfg.Cwd, "cache"),
		TokenFile: cfg.ConfigPath,
		NoRefuse:  cfg.NoRefuse,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}

	agentVariant := "faq (declines off-topic)"
	if cfg.NoRefuse {
		agentVariant = "norefuse (do-what-you're-asked)"
	}
	fmt.Printf("ak-tgclaude: scaffold materialized\n")
	fmt.Printf("  cwd:      %s\n", cfg.Cwd)
	fmt.Printf("  settings: %s\n", filepath.Join(cfg.Cwd, ".claude", "settings.json"))
	fmt.Printf("  agent:    %s\n", agentVariant)
	fmt.Printf("  outbox:   %s\n", outboxRoot)
	if cfg.ConfigPath == "" {
		fmt.Printf("  (no --config given: the token guard has no deny-read path)\n")
	}
	agentFlag := ""
	if cfg.Agent != "" {
		agentFlag = " --agent " + cfg.Agent
	}
	fmt.Printf("\nrun claude there by hand to observe the sandbox (the --settings\n")
	fmt.Printf("overlay grants write to just this outbox, as the dispatcher does per invocation):\n")
	fmt.Printf("  cd %s\n", cfg.Cwd)
	fmt.Printf("  AK_TGCLAUDE_OUTBOX=%s claude -p --setting-sources project --permission-mode dontAsk \\\n", outboxRoot)
	fmt.Printf("    --settings '%s'%s 'hello'\n", buildInvocationSettings(outboxRoot), agentFlag)
}
