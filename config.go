package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// int64List is a repeatable integer flag (e.g. --allow-user 1 --allow-user 2).
type int64List []int64

func (l *int64List) String() string {
	parts := make([]string, len(*l))
	for i, v := range *l {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(parts, ",")
}

func (l *int64List) Set(s string) error {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid user id %q: %w", s, err)
	}
	*l = append(*l, v)
	return nil
}

// stringList is a repeatable string flag (e.g. --wire-skill a --wire-skill b).
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

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

	// WireSkills lists skill templates to materialize into the responder scaffold
	// and preload into the built-in agent. Each entry is a path to a skill
	// directory (containing SKILL.md) or a SKILL.md file. A leading ~ is expanded;
	// a relative path resolves against the dispatcher's launch cwd (like Project/
	// Cwd) — the template may live outside the project tree, so it is never
	// resolved against the project. Any {{PROJECT}} in the template body is
	// replaced with the project path at materialization (Read/Grep do not expand
	// $VARS in tool paths, so a wired skill hard-codes {{PROJECT}}/notes/…). The
	// wired skill's name is appended to the built-in agent's `skills:` so its body
	// is always in context — on-demand skill loading is not guaranteed. Repeatable
	// via --wire-skill (additive with this list).
	WireSkills []string `toml:"wire_skills"`

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

	// BangBug makes the PreToolUse hook deny sandboxed Bash whose command contains
	// a `\!` — the signature of Claude Code bug #64301, where the sandbox
	// blind-escapes `!`→`\!` and silently corrupts the command/output. The denied
	// call is pushed to "write the script to a file". Default false (opt-in); it is
	// a workaround, and a legitimate `\!` (e.g. `find … \!`) would be caught too.
	// Also --bang-bug.
	BangBug bool `toml:"bang_bug"`

	// Bill sends the responder's dollar cost (total_cost_usd from `claude -p`) as a
	// bare "$n.nnn" message to the chat after each answer, but only when that cost
	// is present and non-zero. Under a subscription the figure is notional (what the
	// run would cost at API rates), not real billing. Default false. Also --bill.
	Bill bool `toml:"bill"`

	// Project is the codebase the responder consults on (read-only under "qa").
	// The sandbox and PreToolUse confine the responder's reads here.
	Project string `toml:"project"`

	// DenyRead lists extra paths the responder must never read, on top of the
	// always-denied host secrets (SSH keys, Claude creds/history, other sessions'
	// transcripts, the token file). Each path is denied at BOTH layers: the
	// PreToolUse hook blocks the Read tool (checked before the project-read allow,
	// so it wins even for a path inside the project), and sandbox.filesystem.denyRead
	// blocks the sandboxed Bash (`cat`/`grep`). A leading ~ is expanded and the
	// path is made absolute against the launch cwd (like Project/WireSkills), so
	// the hook's absolute-path match works. Repeatable via --deny-read (additive
	// with this list).
	DenyRead []string `toml:"deny_read"`

	// HelpText is the reply to /help and /start. Empty => a generic built-in
	// blurb (defaultHelpText). Keeps the dispatcher domain-blind: any
	// project-specific help comes from config, not baked into the binary.
	HelpText string `toml:"help_text"`

	// HelpHTML sends HelpText with parse_mode=HTML (Telegram HTML: <b> <i> <a…>,
	// with &<> escaped). Default false = plain text, so a stray &/< in a plain
	// blurb can't break rendering. Set only when help_text is valid Telegram HTML.
	HelpHTML bool `toml:"help_html"`

	// AllowedUsers whitelists the Telegram user ids that may use the bot. Empty
	// (and Open=false) denies everyone — default-closed, so an unconfigured bot is
	// shut, not open. A denied user still gets a "no access for id N" line on
	// /start and /help so they can report the id to be whitelisted. Merged with
	// any --allow-user flags.
	AllowedUsers []int64 `toml:"allowed_users"`

	// Open disables the whitelist — every Telegram user is allowed. Demo only;
	// loudly logged at startup. Also --open. Overrides AllowedUsers.
	Open bool `toml:"open_access"`

	// Cwd is a fixed responder launch dir. When set, the scaffold is materialized
	// there and kept (inspect the generated settings, tweak settings.local, run
	// claude by hand). Empty => an ephemeral cwd the dispatcher removes on exit.
	Cwd string `toml:"cwd"`

	// Workdir is a static, canon-only workspace root. When set, the responder cwd
	// is $Workdir/project — materialized from canon on every start (its contents are
	// reset and regenerated, so unlike Cwd it is NOT a hand-drop overlay) — and the
	// durable session store moves to $Workdir/state. Because $Workdir/project lives
	// at a stable path, it can be marked trusted once in ~/.claude.json (trust is
	// keyed by path, so regenerating the contents keeps it trusted). Mutually
	// exclusive with Cwd (and with RuntimeBase, which only governs an ephemeral cwd).
	// The Go build cache stays under StateDir, shared across bots. Also --workdir.
	Workdir string `toml:"workdir"`

	// RuntimeBase is the base dir under which the ephemeral responder cwd (a
	// pseudo-random subdir) is created. Empty => $XDG_RUNTIME_DIR, else a temp dir.
	RuntimeBase string `toml:"runtime_base"`

	// StateDir holds durable dispatcher state (chat->session, message->session),
	// which must survive restarts. Empty => $XDG_STATE_HOME/ak-tgclaude.
	StateDir string `toml:"state_dir"`

	// EphemeralSessions keeps the chat→session map in memory only: it is never
	// written to disk, so every restart starts each chat fresh. The getUpdates
	// offset still persists (a restart does not reprocess the backlog). Default
	// false (bindings persist). Also --ephemeral-sessions. The `clear` subcommand
	// is the one-shot alternative — wipe persisted bindings without going ephemeral.
	EphemeralSessions bool `toml:"ephemeral_sessions"`

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
	workdir := fs.String("workdir", "", "static canon-only workspace root: $workdir/project is the responder cwd (regenerated from canon, trusted once) and $workdir/state holds the session store (mutually exclusive with --cwd)")
	maxConcurrent := fs.Int("max-concurrent", 0, "max responders running at once (per-chat is always serialized; default 4)")
	noRefuse := fs.Bool("norefuse", false, "materialize the do-what-you're-asked responder (does not decline off-topic; machine guards still apply)")
	ephemeralSessions := fs.Bool("ephemeral-sessions", false, "keep chat→session bindings in memory only (never persisted; offset still persists; each restart starts fresh)")
	bill := fs.Bool("bill", false, "after each answer, send the run's dollar cost as a bare \"$n.nnn\" message (only when present and non-zero)")
	bangBug := fs.Bool("bang-bug", false, `deny sandboxed Bash containing \! (workaround for bug #64301 corrupting the bang char); the responder writes such commands to a file instead`)
	var allowUsers int64List
	fs.Var(&allowUsers, "allow-user", "authorize a Telegram user id (repeatable; merged with allowed_users)")
	var wireSkills stringList
	fs.Var(&wireSkills, "wire-skill", "skill template (dir or SKILL.md) to materialize and preload into the responder (repeatable; merged with wire_skills)")
	var denyRead stringList
	fs.Var(&denyRead, "deny-read", "path the responder must never read, at both the Read-tool and sandboxed-Bash layers (repeatable; merged with deny_read; ~ and relative resolved against the launch cwd)")
	open := fs.Bool("open", false, "OPEN ACCESS: allow every Telegram user (demo only; overrides the whitelist)")
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
	if *workdir != "" {
		c.Workdir = *workdir
	}
	if *maxConcurrent != 0 {
		c.MaxConcurrent = *maxConcurrent
	}
	if *noRefuse {
		c.NoRefuse = true
	}
	if *ephemeralSessions {
		c.EphemeralSessions = true
	}
	if *bill {
		c.Bill = true
	}
	if *bangBug {
		c.BangBug = true
	}
	// allowed_users is additive: --allow-user appends to the file list (rather
	// than overriding it) so the CLI can grant one-off access on top of config.
	if len(allowUsers) > 0 {
		c.AllowedUsers = append(c.AllowedUsers, allowUsers...)
	}
	// wire_skills is additive too, for the same reason (grant one skill ad-hoc).
	if len(wireSkills) > 0 {
		c.WireSkills = append(c.WireSkills, wireSkills...)
	}
	// deny_read is additive too (protect one more path ad-hoc).
	if len(denyRead) > 0 {
		c.DenyRead = append(c.DenyRead, denyRead...)
	}
	if *open {
		c.Open = true
	}

	c.applyDefaults()
	// Every path is expanded (~) and made absolute against the launch cwd, so it
	// is unambiguous once the responder consumes it from the scaffold cwd. This is
	// also the token file's deny-read path (ConfigPath), so a relative --config
	// still matches in the hook.
	c.Project = resolvePath(c.Project)
	c.Cwd = resolvePath(c.Cwd)
	c.Workdir = resolvePath(c.Workdir)
	c.StateDir = resolvePath(c.StateDir)
	c.RuntimeBase = resolvePath(c.RuntimeBase)
	c.ConfigPath = resolvePath(c.ConfigPath)
	for i := range c.WireSkills {
		c.WireSkills[i] = resolvePath(c.WireSkills[i])
	}
	for i := range c.DenyRead {
		c.DenyRead[i] = resolvePath(c.DenyRead[i])
	}

	// Fail fast on a path we cannot represent literally in the sandbox glob rules
	// or the hook shell command, rather than silently mis-match (dangerous for a
	// deny-read).
	for _, pv := range []struct{ field, path string }{
		{"project", c.Project},
		{"cwd", c.Cwd},
		{"workdir", c.Workdir},
		{"state_dir", c.StateDir},
		{"runtime_base", c.RuntimeBase},
		{"config", c.ConfigPath},
	} {
		if err := validatePath(pv.field, pv.path); err != nil {
			return nil, err
		}
	}
	for i, s := range c.WireSkills {
		if err := validatePath(fmt.Sprintf("wire_skills[%d]", i), s); err != nil {
			return nil, err
		}
	}
	for i, s := range c.DenyRead {
		if err := validatePath(fmt.Sprintf("deny_read[%d]", i), s); err != nil {
			return nil, err
		}
	}

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
	if c.Workdir != "" {
		if c.Cwd != "" {
			return fmt.Errorf("cwd and workdir are mutually exclusive: workdir owns a canon-only $workdir/project, cwd is a separate kept, hand-editable dir")
		}
		if c.RuntimeBase != "" {
			return fmt.Errorf("runtime_base is meaningless with workdir: the responder cwd is the fixed $workdir/project, never ephemeral")
		}
	}
	return nil
}

// SessionDir is where the durable session store (getUpdates offset + chat→session
// map, in sessions.json) lives. With Workdir set it is $Workdir/state (per-bot,
// beside project); otherwise it is StateDir (the default location). The Go build
// cache is deliberately NOT here — it stays under StateDir/cache, shared across
// bots, so it never follows a per-bot workdir.
func (c *Config) SessionDir() string {
	if c.Workdir != "" {
		return filepath.Join(c.Workdir, "state")
	}
	return c.StateDir
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

// pathGlobMeta are the fnmatch/gitignore glob metacharacters (plus the `\`
// escape) that must not appear literally in a configured path: the sandbox
// filesystem rules (denyRead/allowWrite/credentials) glob-match, so a literal
// `*` in a deny-read would match as a pattern and silently protect the wrong
// files. We reject rather than escape (no portable literal-escape across the
// shell, fnmatch, and JSON sinks). Spaces and quotes are allowed — shellQuote
// handles the shell command and fnmatch treats them literally.
const pathGlobMeta = `*?[]\`

// validatePath fails fast on a resolved config path we cannot represent
// literally downstream (glob metacharacter or control character). field names
// the offending config key for the error.
func validatePath(field, p string) error {
	if p == "" {
		return nil
	}
	for i := 0; i < len(p); i++ {
		if b := p[i]; b < 0x20 || b == 0x7f {
			return fmt.Errorf("%s path %q contains a control character (0x%02x) — not supported", field, p, b)
		}
	}
	if i := strings.IndexAny(p, pathGlobMeta); i >= 0 {
		return fmt.Errorf("%s path %q contains %q, a glob metacharacter the sandbox would treat as a pattern — use a literal path (symlink or rename to avoid it)", field, p, p[i:i+1])
	}
	return nil
}

// resolvePath expands a leading ~ and makes the path absolute against the
// dispatcher's LAUNCH cwd. Every config path is resolved this way so it is
// unambiguous once the responder consumes it from a different cwd (the scaffold
// dir) — its hook, sandbox, and {{PROJECT}} substitution all need absolute
// paths, and a relative path would otherwise resolve against the responder cwd,
// not where the operator ran the bot. Empty stays empty (an unset optional path
// must not become the launch cwd).
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	p = expandTilde(p)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
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
