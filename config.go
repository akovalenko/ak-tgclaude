package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile selects the responder's access level. Only ProfileQA (read-only) is
// implemented for now; ProfileDev/ProfileOps are reserved for a future
// remote-development pivot.
type Profile string

const (
	ProfileQA  Profile = "qa"
	ProfileDev Profile = "dev"
	ProfileOps Profile = "ops"
)

// Responder implementations.
const (
	ResponderClaude = "claude" // spawn `claude -p` (production)
	ResponderStub   = "stub"   // fixed-reply stub for Telegram I/O smoke tests
)

// defaultAgent is the responder agent shipped in the scaffold (assets/agents).
const defaultAgent = "faq-responder"

// Config is the resolved dispatcher configuration, populated from a TOML file
// and/or CLI flags. Precedence: flags > file > defaults.
type Config struct {
	// BotToken is the Telegram bot token. Held only in the dispatcher's memory,
	// never exported to the environment. Inline in TOML or via --bot-token.
	BotToken string `toml:"bot_token"`

	// Profile is the responder access profile (default "qa", read-only).
	Profile Profile `toml:"profile"`

	// Agent is the responder agent name passed to `claude -p --agent`. Empty
	// uses the cwd's configured default agent.
	Agent string `toml:"agent"`

	// Responder selects the responder implementation: "claude" (default) spawns
	// `claude -p`; "stub" replies with a fixed line, for smoke-testing the
	// Telegram I/O path without a model or scaffold.
	Responder string `toml:"responder"`

	// MaxConcurrent caps how many responders run at once. Updates are serialized
	// per chat, but different chats run concurrently up to this bound. Default 4.
	MaxConcurrent int `toml:"max_concurrent"`

	// NoRefuse materializes the do-what-you're-asked responder variant (does not
	// decline off-topic messages). Machine guards still apply, so it cannot
	// exceed the read-only sandboxed contract.
	NoRefuse bool `toml:"no_refuse"`

	// Project is the codebase the responder consults on (read-only under "qa").
	// The sandbox and PreToolUse confine the responder's reads here.
	Project string `toml:"project"`

	// Cwd is a fixed responder launch dir. When set, the scaffold is materialized
	// there and kept (inspect the generated settings, tweak settings.local, run
	// claude by hand). Empty => an ephemeral cwd the dispatcher removes on exit.
	Cwd string `toml:"cwd"`

	// RuntimeBase is the base dir under which the ephemeral responder cwd (a
	// pseudo-random subdir) is created. Empty => $XDG_RUNTIME_DIR, else a temp dir.
	RuntimeBase string `toml:"runtime_base"`

	// StateDir holds durable dispatcher state (chat->session, message->session),
	// which must survive restarts. Empty => $XDG_STATE_HOME/ak-tgclaude.
	StateDir string `toml:"state_dir"`

	// ConfigPath is the path of the loaded TOML config, if any. Set at load time
	// (not a config field). When the token lives in this file, it is registered
	// for sandbox deny-read in the responder's scaffold.
	ConfigPath string `toml:"-"`
}

// loadConfig resolves configuration (parseConfig) and validates it for the
// dispatcher (bot token, project, ...).
func loadConfig(args []string) (*Config, error) {
	c, err := parseConfig(args)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// parseConfig resolves configuration from an optional TOML file overlaid with
// CLI flags (flags > file > defaults), without dispatcher-specific validation.
// The scaffold subcommand reuses it (it needs a cwd, not a token).
func parseConfig(args []string) (*Config, error) {
	fs := flag.NewFlagSet("ak-tgclaude", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to a TOML config file (optional; flags override it)")
	botToken := fs.String("bot-token", "", "Telegram bot token (overrides config; visible in host ps — prefer the config file in production)")
	profile := fs.String("profile", "", "access profile: qa|dev|ops (default qa, read-only)")
	project := fs.String("project", "", "path to the project the responder consults on (read-only)")
	agent := fs.String("agent", "", "responder agent name for `claude -p --agent` (default: the shipped faq-responder)")
	responder := fs.String("responder", "", "responder implementation: claude|stub (default claude; stub replies a fixed line for Telegram I/O tests)")
	cwd := fs.String("cwd", "", "fixed responder cwd to materialize into and keep (default: ephemeral, removed on exit)")
	maxConcurrent := fs.Int("max-concurrent", 0, "max responders running at once (per-chat is always serialized; default 4)")
	noRefuse := fs.Bool("norefuse", false, "materialize the do-what-you're-asked responder (does not decline off-topic; machine guards still apply)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	var c Config
	if *configPath != "" {
		if _, err := toml.DecodeFile(*configPath, &c); err != nil {
			return nil, fmt.Errorf("reading config %s: %w", *configPath, err)
		}
		c.ConfigPath = *configPath
	}
	// Flags override file values when set.
	if *botToken != "" {
		c.BotToken = *botToken
	}
	if *profile != "" {
		c.Profile = Profile(*profile)
	}
	if *project != "" {
		c.Project = *project
	}
	if *agent != "" {
		c.Agent = *agent
	}
	if *responder != "" {
		c.Responder = *responder
	}
	if *cwd != "" {
		c.Cwd = *cwd
	}
	if *maxConcurrent != 0 {
		c.MaxConcurrent = *maxConcurrent
	}
	if *noRefuse {
		c.NoRefuse = true
	}

	c.applyDefaults()
	c.Project = expandTilde(c.Project)
	c.Cwd = expandTilde(c.Cwd)
	c.StateDir = expandTilde(c.StateDir)
	c.RuntimeBase = expandTilde(c.RuntimeBase)

	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Profile == "" {
		c.Profile = ProfileQA
	}
	if c.Responder == "" {
		c.Responder = ResponderClaude
	}
	if c.Agent == "" {
		c.Agent = defaultAgent
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	// RuntimeBase is resolved at cwd-materialization time (it depends on whether
	// $XDG_RUNTIME_DIR exists when the dispatcher starts), so it stays empty here.
}

func (c *Config) validate() error {
	if c.BotToken == "" {
		return fmt.Errorf("bot token is required (bot_token in config or --bot-token)")
	}
	switch c.Profile {
	case ProfileQA:
		// ok
	case ProfileDev, ProfileOps:
		return fmt.Errorf("profile %q is reserved but not implemented yet (only %q works)", c.Profile, ProfileQA)
	default:
		return fmt.Errorf("unknown profile %q (want qa|dev|ops)", c.Profile)
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("max_concurrent must be >= 1, got %d", c.MaxConcurrent)
	}
	switch c.Responder {
	case ResponderClaude:
		// The claude responder consults a project; the stub does not need one.
		if c.Project == "" {
			return fmt.Errorf("project is required (project in config or --project)")
		}
	case ResponderStub:
		// ok — no project needed
	default:
		return fmt.Errorf("unknown responder %q (want claude|stub)", c.Responder)
	}
	return nil
}

// defaultStateDir is $XDG_STATE_HOME/ak-tgclaude, else ~/.local/state/ak-tgclaude.
func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "ak-tgclaude")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "ak-tgclaude")
	}
	return ".ak-tgclaude-state"
}

// expandTilde expands a leading ~ or ~/ to the user's home directory.
func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return h
	}
	return filepath.Join(h, p[2:])
}

// redact masks a secret for display, keeping only a short suffix.
func redact(s string) string {
	if s == "" {
		return "(unset)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}
