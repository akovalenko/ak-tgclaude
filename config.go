package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// policyList is the ordered list of persona-fragment selectors composed into one
// persona (injected at spawn via --append-system-prompt). It decodes from TOML as
// EITHER an array (policies = ["norefuse", "introspect"] — the convention) or a
// bare string (policies = "norefuse"), both yielding the same []string. On the CLI
// it is the repeatable --policy flag (a stringList), additive with the file list.
type policyList []string

func (pl *policyList) UnmarshalTOML(v interface{}) error {
	switch x := v.(type) {
	case string:
		*pl = policyList{x}
	case []interface{}:
		out := make(policyList, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("policy list entry %v is not a string", e)
			}
			out = append(out, s)
		}
		*pl = out
	default:
		return fmt.Errorf("policy must be a string or an array of strings, got %T", v)
	}
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

	// ClaudeArgs are extra raw arguments appended to the responder's `claude -p`
	// invocation — e.g. ["--model", "opus", "--effort", "high"]. They pass through
	// verbatim, so any current or future claude flag works without a dedicated
	// knob. Flags ak-tgclaude owns (the security, MCP-transport, session, and
	// print/format flags — see claudeArgDenylist) are REJECTED at startup rather
	// than allowed to silently override the sandbox/transport ak-tgclaude sets;
	// everything else (model, effort, verbosity, …) passes. Repeatable via
	// --claude-arg (additive with this list).
	ClaudeArgs []string `toml:"claude_args"`

	// WireSkills lists skill templates to materialize into the responder scaffold
	// and preload into the built-in agent. Each entry is a path to a skill
	// DIRECTORY (containing SKILL.md): the whole tree is copied, so bundled
	// resources (reference.md, scripts, selftest) come along and executable bits
	// are preserved. A leading ~ is expanded; a relative path resolves against the
	// dispatcher's launch cwd (like Project/Workdir) — the template may live
	// outside the project tree, so it is never resolved against the project. Any
	// {{PROJECT}} in the template body is
	// replaced with the project path at materialization (Read/Grep do not expand
	// $VARS in tool paths, so a wired skill hard-codes {{PROJECT}}/notes/…). The
	// wired skill's name is appended to the built-in agent's `skills:` so its body
	// is always in context — on-demand skill loading is not guaranteed. Repeatable
	// via --wire-skill (additive with this list).
	WireSkills []string `toml:"wire_skills"`

	// AddSkills lists GENERIC skill directories to copy verbatim into the scaffold
	// (no {{PROJECT}} substitution, not preloaded into the agent). Unlike wire_skills
	// — which bind a single domain skill always into context — these sit in
	// .claude/skills/ for on-demand use: the responder sees their descriptions (the
	// skill "table of contents") and pulls one in via the Skill tool when relevant.
	// Same path rules as wire_skills (~ expanded, relative to launch cwd; may live
	// outside the project). Repeatable via --add-skill (additive with this list).
	AddSkills []string `toml:"add_skills"`

	// AddAgents lists GENERIC agent .md FILES to copy verbatim into the scaffold's
	// .claude/agents/ (no substitution, not preloaded). They become subagent types
	// the responder may delegate to on demand. Same path rules as add_skills.
	// Repeatable via --add-agent (additive with this list).
	AddAgents []string `toml:"add_agents"`

	// Tools grants EXTRA tools to the responder. Each value is split across the two
	// grants that must move together: its bare NAME goes into the agent's `tools:`
	// frontmatter (availability), and the value VERBATIM goes into `--allowedTools`
	// (permission). So a scoped spec like "WebFetch(domain:*.github.com)" grants bare
	// WebFetch availability plus that exact domain scope in one knob — and two specs
	// with the same verb (WebFetch(domain:a), WebFetch(domain:b)) collapse to a single
	// WebFetch in the frontmatter while both scopes ride --allowedTools. Values are
	// tool names, scoped specs, or MCP patterns (e.g. "Agent", "WebFetch(domain:X)",
	// "mcp__x__*"). A sharp, operator-only knob for ad-hoc grants: the sandbox still
	// confines Bash and the PreToolUse hook still gates the file tools, but a tool the
	// hook does NOT gate (WebFetch, Agent, …) genuinely widens access — grant
	// deliberately. Repeatable via --tool (additive with this list).
	Tools []string `toml:"tools"`

	// Responder selects the responder implementation: "claude" (default) spawns
	// `claude -p`; "stub" replies with a fixed line, for smoke-testing the
	// Telegram I/O path without a model or scaffold.
	Responder string `toml:"responder"`

	// MaxConcurrent caps how many responders run at once. Updates are serialized
	// per chat, but different chats run concurrently up to this bound. Default 4.
	MaxConcurrent int `toml:"max_concurrent"`

	// OutboxTTL is how long a chat's PERSISTENT outbox (its working dir, reattached
	// across the chat's turns so the responder needn't rebuild what it built earlier)
	// is kept after last use before idle-eviction reaps it. A Go duration ("2h",
	// "30m"); default 2h; "0" disables eviction (outboxes then live until /clear, a
	// session reset, or shutdown under ephemeral sessions).
	OutboxTTL string `toml:"outbox_ttl"`

	// Policies selects the responder's DEFAULT persona/stance, composed and injected
	// at spawn via --append-system-prompt. Each entry is a built-in name — "normal"
	// (declines off-topic, the default), "norefuse" (do-what-you're-asked), "strict"
	// (hard-scoped, refuses anything but direct project questions), "introspect"
	// (candid/debug: precise about failures, explains the machinery) — OR a path to a
	// custom .md fragment. Multiple entries are MERGED in order (blank-line separated)
	// into one persona, so stances can be layered. A fragment may declare `axis:` in
	// its frontmatter; two default entries sharing an axis fail at load. TOML accepts
	// an array (the plural-key convention) or a bare string; --policy is repeatable
	// and additive with the file list. Per-user tweaks go in PolicyOverrides. Machine
	// guards still apply, so no persona (or blend) can exceed the read-only sandboxed
	// contract.
	Policies policyList `toml:"policies"`

	// PolicyOverrides maps a Telegram user id (the TOML table key, e.g.
	// [policy_overrides] then 12345 = ["norefuse"]) to a per-user persona override.
	// The override is layered on top of Policies ALONG AXES: an override fragment
	// that declares an axis EVICTS the default fragment on that same axis, while an
	// axis-less fragment is appended. So a default of ["strict"] with an override of
	// ["norefuse"] yields ["norefuse"] for that user; a default of ["strict", "rw"]
	// with ["norefuse"] yields ["norefuse", "rw"]. Same fragment vocabulary as
	// Policies (built-in names and/or custom .md paths).
	PolicyOverrides map[string]policyList `toml:"policy_overrides"`

	// Owner is a Telegram user id treated as the bot's owner: it is auto-whitelisted
	// (added to AllowedUsers) and, unless it has an explicit PolicyOverrides entry,
	// granted the relaxed owner persona (ownerPolicies — norefuse + introspect). One
	// knob for "owner = admin"; the id must be supplied (the Bot API's getMe does not
	// reveal the bot's owner). Also --owner.
	Owner int64 `toml:"owner"`

	// overrides holds the RESOLVED per-user persona selector lists (Policies layered
	// with each user's override along axes), keyed by Telegram user id; built in Load
	// after validation. An absent key means the default Policies apply. Not decoded
	// from TOML — derived. Read it via PersonaSelectors.
	overrides map[int64][]string

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

	// AllowSilent DISABLES the delivery guard. The guard (on by default) catches a
	// responder that ended without calling any send tool — a weaker model sometimes
	// dumps its answer into its final text, which is only the discarded status
	// signal, so the user gets nothing. When guarded, the dispatcher re-prompts the
	// same session once to actually deliver, then falls back to UndeliveredText. Set
	// this (or --allow-silent) only if a no-send turn is legitimate for your bot.
	// Default false (guard on). The field is inverted (allow_silent, not
	// require_delivery) so the safe default needs no config and the CLI never needs
	// --flag=false to turn a default-on bool off.
	AllowSilent bool `toml:"allow_silent"`

	// UndeliveredText is the fallback reply sent when the delivery guard is active
	// and the responder STILL delivered nothing after the re-prompt. Empty => no
	// fallback message (the guard then only re-prompts and logs). Ignored when
	// AllowSilent is set. Plain text.
	UndeliveredText string `toml:"undelivered_text"`

	// Debug turns on troubleshooting output, all to the dispatcher log: it passes
	// `--debug` to the responder's `claude -p` (its own diagnostics — MCP handshake/
	// tool discovery/transport errors — go to the responder's stderr, which the
	// dispatcher inherits), dumps the responder's full final text after each run, and
	// on a chat's FIRST spawn logs the persona that user resolved to (the selector
	// label, e.g. [normal] vs [norefuse], and the composed --append-system-prompt
	// text). Verbose; for troubleshooting only. Default false. Also --debug.
	Debug bool `toml:"debug"`

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
	DenyRead []string `toml:"deny_reads"`

	// DenyEnvs lists additional environment-variable NAMES to scrub from the
	// responder's sandboxed shell, on top of the always-denied defaults
	// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN). Use it for extra host secrets that
	// would otherwise leak via the environment. These are variable names, not
	// paths — no ~/relative resolution or path validation. Repeatable via
	// --deny-env (additive with this list).
	DenyEnvs []string `toml:"deny_envs"`

	// AllowDomains lists EXTRA egress domains added to the responder's sandbox
	// network allowlist (sandbox.network.allowedDomains), on top of the always-present
	// Go-build defaults (proxy.golang.org, sum.golang.org, storage.googleapis.com).
	// This is the sandboxed-Bash egress layer — a responder `curl`/`go get` may reach
	// the listed host — SEPARATE from a WebFetch(domain:X) tool grant (see Tools),
	// which scopes the WebFetch tool and, under `claude -p`, does NOT open sandbox
	// egress. A leading `*.` matches subdomains only, not the apex (list the apex too
	// if you need it). Additive and de-duplicated (the Go defaults are never dropped).
	// Repeatable via --allow-domain (additive with this list).
	AllowDomains []string `toml:"allow_domains"`

	// UploadCommand enables the large-file fallback for send_document. It is a path
	// to an operator UPLOADER script invoked as argv [command, <file>, <name>]:
	// <file> is the local path to upload, <name> is a collision-free basename (a
	// random prefix + the original name, e.g. a3f9c2-dist.tar.gz) the uploader MAY
	// use as the destination name so concurrent same-named files don't clobber each
	// other on the share host. An uploader that doesn't need it can ignore arg2 as
	// long as it does not REJECT a second argument (a strict one-arg script must be
	// relaxed). The name preserves the original filename, so it may hold non-ASCII
	// (e.g. Cyrillic); an uploader that builds a URL should percent-encode it (see
	// examples/rsync-upload.sh). The command must
	// print the file's public URL on stdout (first non-blank line) and exit 0, or
	// exit non-zero with a message on stderr. When set, a document larger than
	// UploadThresholdMB is uploaded via this command (run UNSANDBOXED by the
	// dispatcher — it needs the network) and delivered to the chat as that URL
	// instead of a Telegram attachment (which caps near 50 MB). Empty => off. The
	// referenced file stays confined to the responder's outbox; the command is
	// operator trust. Path (leading ~ expanded). Also --upload-command.
	UploadCommand string `toml:"upload_command"`

	// UploadThresholdMB is the size (MB) above which a document goes via UploadCommand
	// instead of a direct Telegram attachment. Default 40 (headroom under Telegram's
	// ~50 MB bot limit). Ignored when UploadCommand is empty. Also --upload-threshold-mb.
	UploadThresholdMB int `toml:"upload_threshold_mb"`

	// UploadMaxMB is the ADVERTISED ceiling (MB) surfaced to the responder in the
	// tg-emit skill ("you can send files up to N MB"). Enforcement sits slightly
	// above it — a file larger than UploadMaxMB + 10% headroom is rejected with a
	// clear "too large even for the cloud" error rather than handed to the uploader,
	// so a file a touch over the advertised number still goes through. 0 => no
	// advertised number and no hard cap (only the threshold routing applies).
	// Ignored when UploadCommand is empty. Also --upload-max-mb.
	UploadMaxMB int `toml:"upload_max_mb"`

	// MaxIncomingMB caps the size (MB) of an incoming document the bot downloads
	// into the responder's outbox; a larger attachment is refused with a note to
	// the user instead. Default 20 — the Telegram bot-API getFile ceiling, above
	// which the bot cannot fetch a file at all. Also --max-incoming-mb.
	MaxIncomingMB int `toml:"max_incoming_mb"`

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

	// Workdir is a static, canon-only workspace root. When set, the responder cwd
	// is $Workdir/project — materialized from canon on every start (its contents are
	// reset and regenerated, so it is NOT a hand-drop overlay) — and the durable
	// session store moves to $Workdir/state. Because $Workdir/project lives at a
	// stable path, it can be marked trusted once in ~/.claude.json (trust is keyed by
	// path, so regenerating the contents keeps it trusted). Empty => an ephemeral cwd
	// the dispatcher removes on exit. Mutually exclusive with RuntimeBase (which only
	// governs the ephemeral cwd). The Go build cache stays under StateDir, shared
	// across bots. Also --workdir.
	Workdir string `toml:"workdir"`

	// RuntimeBase is the base dir under which the ephemeral responder cwd (a
	// pseudo-random subdir) is created. Empty => $XDG_RUNTIME_DIR, else a temp dir.
	RuntimeBase string `toml:"runtime_base"`

	// StateDir holds durable dispatcher state (chat->session, message->session),
	// which must survive restarts. Empty => $XDG_STATE_HOME/ak-tgclaude.
	StateDir string `toml:"state_dir"`

	// Transcripts enables the per-chat transcript store (transcript.go) and the
	// tg-recall skill. Default OFF: it writes users' message text to disk, so it is
	// opt-in for privacy. When off, nothing is recorded and tg-recall is not shipped
	// to the responder. Also --transcripts.
	Transcripts bool `toml:"transcripts"`

	// TranscriptDir overrides the store root. Empty => <SessionDir>/transcripts
	// (Workdir/state or StateDir). It must live OUTSIDE the responder outbox so it
	// survives the session-TTL wipe (validate rejects a root under the project/workdir).
	// Also --transcript-dir.
	TranscriptDir string `toml:"transcript_dir"`

	// OwnerReadsAll: when transcripts are on, the owner's responder reads the WHOLE
	// transcripts root (cross-chat analytics) rather than just its own chat. nil =>
	// default true; false scopes the owner like any other user. A *bool so the
	// default-true survives an unset config. Also --owner-reads-all.
	OwnerReadsAll *bool `toml:"owner_reads_all"`

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
	var claudeArgs stringList
	fs.Var(&claudeArgs, "claude-arg", "extra raw argument appended to the responder's `claude -p` (one token each, e.g. --claude-arg=--model --claude-arg=opus); repeatable, merged with claude_args; ak-tgclaude-owned flags are rejected")
	claudeArgsStr := fs.String("claude-args", "", "same as --claude-arg but as ONE whitespace-split string (e.g. --claude-args \"--model opus --effort high\"); merged with claude_args and --claude-arg (a flag value with a space needs --claude-arg instead)")
	responder := fs.String("responder", "", "responder implementation: claude|stub (default claude; stub replies a fixed line for Telegram I/O tests)")
	workdir := fs.String("workdir", "", "static canon-only workspace root: $workdir/project is the responder cwd (regenerated from canon each start, trusted once) and $workdir/state holds the session store (default: an ephemeral cwd, removed on exit)")
	maxConcurrent := fs.Int("max-concurrent", 0, "max responders running at once (per-chat is always serialized; default 4)")
	outboxTTL := fs.String("outbox-ttl", "", `how long an idle chat's persistent outbox is kept before eviction (Go duration, e.g. "2h"; "0" disables; default 2h)`)
	var policyFlags stringList
	fs.Var(&policyFlags, "policy", "DEFAULT responder persona composed into the agent: normal (declines off-topic, default) | norefuse (do-what-you're-asked) | strict (refuses anything but direct project questions) | introspect (candid/debug) | outbox-rw (do read-write tasks via the outbox) | a path to a custom .md fragment; repeatable and additive with the config policies list, entries merged in order into one persona. `--policy help` prints the catalog and exits")
	owner := fs.Int64("owner", 0, "Telegram user id treated as the bot owner: auto-whitelisted and granted the relaxed owner persona (norefuse + introspect) unless it has an explicit policy_overrides entry")
	ephemeralSessions := fs.Bool("ephemeral-sessions", false, "keep chat→session bindings in memory only (never persisted; offset still persists; each restart starts fresh)")
	bill := fs.Bool("bill", false, "after each answer, send the run's dollar cost as a bare \"$n.nnn\" message (only when present and non-zero)")
	allowSilent := fs.Bool("allow-silent", false, "DISABLE the delivery guard (on by default): allow a responder turn that sends nothing. Normally a no-send turn is re-prompted once, then answered with undelivered_text")
	debug := fs.Bool("debug", false, "troubleshooting output to the dispatcher log: pass --debug to the responder's `claude -p` (MCP handshake/tool-call transport diagnostics), dump each run's final text, and on a chat's first spawn log the resolved persona (selector label + the composed --append-system-prompt); verbose")
	bangBug := fs.Bool("bang-bug", false, `deny sandboxed Bash containing \! (workaround for bug #64301 corrupting the bang char); the responder writes such commands to a file instead`)
	var allowUsers int64List
	fs.Var(&allowUsers, "allow-user", "authorize a Telegram user id (repeatable; merged with allowed_users)")
	var wireSkills stringList
	fs.Var(&wireSkills, "wire-skill", "skill template DIRECTORY to materialize and preload into the responder (repeatable; merged with wire_skills)")
	var addSkills stringList
	fs.Var(&addSkills, "add-skill", "generic skill DIRECTORY to copy verbatim for on-demand use (not preloaded; repeatable; merged with add_skills)")
	var addAgents stringList
	fs.Var(&addAgents, "add-agent", "generic agent .md FILE to copy verbatim as a subagent (not preloaded; repeatable; merged with add_agents)")
	var tools stringList
	fs.Var(&tools, "tool", "grant an EXTRA tool to the responder: bare name into the agent's tools: frontmatter, full value into --allowedTools (e.g. --tool Agent --tool 'WebFetch(domain:*.github.com)' — quote it so the shell leaves the parens/asterisks alone); repeatable, merged with tools; a sharp operator knob — see docs")
	var denyRead stringList
	fs.Var(&denyRead, "deny-read", "path the responder must never read, at both the Read-tool and sandboxed-Bash layers (repeatable; merged with deny_reads; ~ and relative resolved against the launch cwd)")
	var denyEnvs stringList
	fs.Var(&denyEnvs, "deny-env", "environment-variable NAME to scrub from the responder's sandbox, on top of the ANTHROPIC defaults (repeatable; merged with deny_envs)")
	var allowDomains stringList
	fs.Var(&allowDomains, "allow-domain", "extra egress domain added to the responder's sandbox network allowlist, on top of the Go-build defaults (repeatable; merged with allow_domains; a leading *. matches subdomains only, not the apex)")
	uploadCommand := fs.String("upload-command", "", "path to an uploader script (argv [cmd, file, name]; prints the URL on stdout + exit 0, else non-zero with stderr) — enables the large-file fallback: a document over --upload-threshold-mb is uploaded and delivered as a link")
	uploadThresholdMB := fs.Int("upload-threshold-mb", 0, "size in MB above which a document is uploaded via --upload-command instead of sent as a Telegram attachment (default 40; ignored without --upload-command)")
	uploadMaxMB := fs.Int("upload-max-mb", 0, "advertised max upload size in MB surfaced to the responder; a file over this +10% is rejected as too large (0 = no advertised number / no hard cap)")
	maxIncomingMB := fs.Int("max-incoming-mb", 0, "max size in MB of an incoming document to download into the responder's outbox (0 => default 20, the bot-API getFile ceiling)")
	open := fs.Bool("open", false, "OPEN ACCESS: allow every Telegram user (demo only; overrides the whitelist)")
	transcripts := fs.Bool("transcripts", false, "enable the per-chat transcript store + tg-recall recall (default off; records users' message text to disk)")
	transcriptDir := fs.String("transcript-dir", "", "override the transcript store root (default <state>/transcripts; must be outside the responder outbox)")
	ownerReadsAll := fs.Bool("owner-reads-all", true, "when transcripts are on, let the owner's responder read the whole transcripts root for cross-chat analytics; --owner-reads-all=false scopes the owner to its own chat like any user")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// `--policy help` is a reserved selector: print the built-in catalog and exit,
	// like `--help`. Checked before any config decoding so it works standalone
	// (`ak-tgclaude scaffold --policy help`) without a token/project/config file.
	for _, p := range policyFlags {
		if p == "help" {
			if err := printPolicyCatalog(os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	var c Config
	if *configPath != "" {
		md, err := toml.DecodeFile(*configPath, &c)
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", *configPath, err)
		}
		// The singular `policy` key was renamed to the plural array `policies`; a
		// stale key would otherwise be silently ignored (unknown keys don't error),
		// so flag it explicitly rather than dropping the operator's persona.
		if md.IsDefined("policy") {
			return nil, fmt.Errorf("config %s: key `policy` was renamed to `policies` (an array) — rename it", *configPath)
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
	// claude_args is additive: --claude-arg appends to the file list (a one-off
	// flag on top of config), like the other repeatable lists.
	if len(claudeArgs) > 0 {
		c.ClaudeArgs = append(c.ClaudeArgs, claudeArgs...)
	}
	// --claude-args is a CLI convenience: one whitespace-split string of tokens,
	// appended after --claude-arg. All three sources (claude_args, --claude-arg,
	// --claude-args) are additive; the denylist guard below runs on the merged
	// result. Whitespace-split, so a flag value containing a space must come via
	// --claude-arg / claude_args (whole tokens) instead.
	if strings.TrimSpace(*claudeArgsStr) != "" {
		c.ClaudeArgs = append(c.ClaudeArgs, strings.Fields(*claudeArgsStr)...)
	}
	if *responder != "" {
		c.Responder = *responder
	}
	if *workdir != "" {
		c.Workdir = *workdir
	}
	if *maxConcurrent != 0 {
		c.MaxConcurrent = *maxConcurrent
	}
	if *outboxTTL != "" {
		c.OutboxTTL = *outboxTTL
	}
	// policies is additive too: --policy appends to the default file list, and the
	// entries are merged in order into one persona (like the other repeatable lists).
	if len(policyFlags) > 0 {
		c.Policies = append(c.Policies, policyFlags...)
	}
	// owner is single-valued: a set flag overrides the file (0 = unset).
	if *owner != 0 {
		c.Owner = *owner
	}
	if *transcripts {
		c.Transcripts = true
	}
	if *transcriptDir != "" {
		c.TranscriptDir = *transcriptDir
	}
	// owner_reads_all defaults true (applyDefaults fills nil); only an explicitly
	// passed --owner-reads-all overrides the file value. fs.Visit fires solely for
	// flags actually set, so the CLI can turn it off without a --flag=false quirk.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "owner-reads-all" {
			c.OwnerReadsAll = ownerReadsAll
		}
	})
	if *ephemeralSessions {
		c.EphemeralSessions = true
	}
	if *bill {
		c.Bill = true
	}
	if *allowSilent {
		c.AllowSilent = true
	}
	if *debug {
		c.Debug = true
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
	// add_skills / add_agents are additive as well (drop in one generic ad-hoc).
	if len(addSkills) > 0 {
		c.AddSkills = append(c.AddSkills, addSkills...)
	}
	if len(addAgents) > 0 {
		c.AddAgents = append(c.AddAgents, addAgents...)
	}
	// tools is additive too (grant one extra tool ad-hoc for an experiment).
	if len(tools) > 0 {
		c.Tools = append(c.Tools, tools...)
	}
	// deny_reads is additive too (protect one more path ad-hoc).
	if len(denyRead) > 0 {
		c.DenyRead = append(c.DenyRead, denyRead...)
	}
	// deny_envs is additive too (scrub one more secret env var ad-hoc).
	if len(denyEnvs) > 0 {
		c.DenyEnvs = append(c.DenyEnvs, denyEnvs...)
	}
	// allow_domains is additive too (open one more egress domain ad-hoc).
	if len(allowDomains) > 0 {
		c.AllowDomains = append(c.AllowDomains, allowDomains...)
	}
	// The upload knobs are single-valued: a set flag overrides the file (0/"" = unset).
	if *uploadCommand != "" {
		c.UploadCommand = *uploadCommand
	}
	if *uploadThresholdMB != 0 {
		c.UploadThresholdMB = *uploadThresholdMB
	}
	if *uploadMaxMB != 0 {
		c.UploadMaxMB = *uploadMaxMB
	}
	if *maxIncomingMB != 0 {
		c.MaxIncomingMB = *maxIncomingMB
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
	c.Workdir = resolvePath(c.Workdir)
	c.StateDir = resolvePath(c.StateDir)
	c.TranscriptDir = resolvePath(c.TranscriptDir)
	c.RuntimeBase = resolvePath(c.RuntimeBase)
	c.ConfigPath = resolvePath(c.ConfigPath)
	// UploadCommand is a path (exec'd by the dispatcher, not sandbox-glob-matched, so
	// no validatePath); resolve ~ and make it absolute like every other path.
	c.UploadCommand = resolvePath(c.UploadCommand)
	for i := range c.WireSkills {
		c.WireSkills[i] = resolvePath(c.WireSkills[i])
	}
	for i := range c.AddSkills {
		c.AddSkills[i] = resolvePath(c.AddSkills[i])
	}
	for i := range c.AddAgents {
		c.AddAgents[i] = resolvePath(c.AddAgents[i])
	}
	for i := range c.DenyRead {
		c.DenyRead[i] = resolvePath(c.DenyRead[i])
	}
	// A policy entry given as a PATH (custom fragment) resolves like the other
	// paths; a built-in NAME is left untouched. Same for per-user override fragments.
	for i := range c.Policies {
		if policyIsPath(c.Policies[i]) {
			c.Policies[i] = resolvePath(c.Policies[i])
		}
	}
	for _, ov := range c.PolicyOverrides {
		for i := range ov {
			if policyIsPath(ov[i]) {
				ov[i] = resolvePath(ov[i])
			}
		}
	}

	// Fail fast on a path we cannot represent literally in the sandbox glob rules
	// or the hook shell command, rather than silently mis-match (dangerous for a
	// deny-read).
	for _, pv := range []struct{ field, path string }{
		{"project", c.Project},
		{"workdir", c.Workdir},
		{"state_dir", c.StateDir},
		{"transcript_dir", c.TranscriptDir},
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
	for i, s := range c.AddSkills {
		if err := validatePath(fmt.Sprintf("add_skills[%d]", i), s); err != nil {
			return nil, err
		}
	}
	for i, s := range c.AddAgents {
		if err := validatePath(fmt.Sprintf("add_agents[%d]", i), s); err != nil {
			return nil, err
		}
	}
	for i, s := range c.DenyRead {
		if err := validatePath(fmt.Sprintf("deny_reads[%d]", i), s); err != nil {
			return nil, err
		}
	}

	// Each policy entry (default or per-user override) is either a built-in name or a
	// readable custom fragment file — fail at startup on an unknown name or a missing
	// file, not mid-run.
	validatePolicyEntry := func(field, p string) error {
		if policyIsPath(p) {
			if err := validatePath(field, p); err != nil {
				return err
			}
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("policy fragment %s: %w", p, err)
			}
		} else if !builtinPolicies[p] {
			return fmt.Errorf("unknown policy %q (built-in: %s; or a path to a .md fragment)", p, strings.Join(builtinPolicyOrder, ", "))
		}
		return nil
	}
	for i, p := range c.Policies {
		if err := validatePolicyEntry(fmt.Sprintf("policies[%d]", i), p); err != nil {
			return nil, err
		}
	}
	if err := checkAxisConflicts(c.Policies); err != nil {
		return nil, fmt.Errorf("default policies: %w", err)
	}
	// Floor the default persona on the refusal axis: an axis-less-only list (a lone
	// custom fragment, or outbox-rw) would leave it with no base FAQ stance, so prepend
	// normal unless a refusal-axis fragment is already present. Done here, before the
	// override resolution below, so every per-user override layers on a based persona too.
	floored, err := withDefaultStance(c.Policies)
	if err != nil {
		return nil, fmt.Errorf("default policies: %w", err)
	}
	c.Policies = floored
	// Per-user overrides: validate the key is a user id and each fragment, guard the
	// override's own axes, then precompute the effective (default-layered) selector
	// list the dispatcher hands to --append-system-prompt.
	if len(c.PolicyOverrides) > 0 {
		c.overrides = make(map[int64][]string, len(c.PolicyOverrides))
		for key, ov := range c.PolicyOverrides {
			uid, err := strconv.ParseInt(key, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("policy_overrides key %q is not a Telegram user id: %w", key, err)
			}
			for i, p := range ov {
				if err := validatePolicyEntry(fmt.Sprintf("policy_overrides[%s][%d]", key, i), p); err != nil {
					return nil, err
				}
			}
			if err := checkAxisConflicts(ov); err != nil {
				return nil, fmt.Errorf("policy_overrides[%s]: %w", key, err)
			}
			eff, err := resolveEffectivePolicies(c.Policies, ov)
			if err != nil {
				return nil, fmt.Errorf("policy_overrides[%s]: %w", key, err)
			}
			c.overrides[uid] = eff
		}
	}

	// Reject any operator claude_arg that names a flag ak-tgclaude owns, so a stray
	// passthrough cannot silently weaken the sandbox or break the transport.
	if err := validateClaudeArgs(c.ClaudeArgs); err != nil {
		return nil, err
	}

	return &c, nil
}

// claudeArgDenylist are the `claude -p` flags ak-tgclaude sets itself: the
// security gate (--permission-mode, --setting-sources, the skip-permissions
// escapes), the MCP transport (--mcp-config, --strict-mcp-config, --allowedTools),
// the per-invocation --settings overlay, the persona injection
// (--append-system-prompt and the --system-prompt* family), the session flags the
// dispatcher manages (--agent, --resume/-r, --continue/-c), and the print/format
// flags it parses
// (-p/--print, --output-format, --input-format). An operator claude_arg naming one
// is rejected at startup: claude's duplicate-flag precedence is undocumented, so
// letting it through could silently override the sandbox/transport or break output
// parsing rather than predictably win. Everything NOT here (--model, --effort,
// --verbose, --add-dir, …) passes through untouched.
var claudeArgDenylist = map[string]bool{
	"-p": true, "--print": true,
	"--output-format": true, "--input-format": true,
	"--setting-sources":      true,
	"--permission-mode":      true,
	"--mcp-config":           true,
	"--strict-mcp-config":    true,
	"--allowedTools":         true,
	"--settings":             true,
	"--append-system-prompt": true,
	"--system-prompt":        true,
	"--system-prompt-file":   true,
	"--agent":                true,
	"--resume":               true, "-r": true,
	"--continue": true, "-c": true,
	"--dangerously-skip-permissions":       true,
	"--allow-dangerously-skip-permissions": true,
}

// validateClaudeArgs rejects any passthrough token that names an ak-tgclaude-owned
// flag (see claudeArgDenylist). It matches both `--flag value` and `--flag=value`
// forms (the token before '='); a bare value (not starting with '-') is a flag's
// argument and is not itself checked.
func validateClaudeArgs(args []string) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			flag = a[:i]
		}
		if claudeArgDenylist[flag] {
			return fmt.Errorf("claude_arg %q is managed by ak-tgclaude and cannot be overridden "+
				"(it governs the sandbox, MCP transport, session, or output format) — remove it", flag)
		}
	}
	return nil
}

// ownerPolicies is the persona granted to the configured owner (Owner / --owner):
// relaxed (norefuse) plus introspective. Applied unless an explicit policy_overrides
// entry for the owner's id says otherwise.
var ownerPolicies = []string{"norefuse", "introspect"}

// containsInt64 reports whether xs contains v.
func containsInt64(xs []int64, v int64) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// PersonaSelectors returns the resolved persona fragment selectors for a user: the
// per-user override (Policies layered with the user's fragments along axes) when one
// is configured, else the default Policies. The dispatcher loads+merges these into
// the --append-system-prompt text on a fresh spawn.
func (c *Config) PersonaSelectors(userID int64) []string {
	if sel, ok := c.overrides[userID]; ok {
		return sel
	}
	return c.Policies
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
	if len(c.Policies) == 0 {
		c.Policies = policyList{defaultPolicy}
	}
	// Owner sugar: auto-whitelist the id and, unless it has an explicit override,
	// grant it the relaxed owner persona. Applied here (before path resolution and
	// override validation) so the owner flows through the same machinery as any
	// other whitelisted user / override entry.
	if c.Owner != 0 {
		if !containsInt64(c.AllowedUsers, c.Owner) {
			c.AllowedUsers = append(c.AllowedUsers, c.Owner)
		}
		key := strconv.FormatInt(c.Owner, 10)
		if _, ok := c.PolicyOverrides[key]; !ok {
			if c.PolicyOverrides == nil {
				c.PolicyOverrides = map[string]policyList{}
			}
			c.PolicyOverrides[key] = append(policyList(nil), ownerPolicies...)
		}
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.OutboxTTL == "" {
		c.OutboxTTL = "2h"
	}
	if c.MaxIncomingMB == 0 {
		c.MaxIncomingMB = 20
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	if c.OwnerReadsAll == nil {
		t := true
		c.OwnerReadsAll = &t
	}
	// Default the upload threshold only when the fallback is enabled — 40 MB leaves
	// headroom under Telegram's ~50 MB bot-attachment limit.
	if c.UploadCommand != "" && c.UploadThresholdMB == 0 {
		c.UploadThresholdMB = 40
	}
	// RuntimeBase is resolved at cwd-materialization time (it depends on whether
	// $XDG_RUNTIME_DIR exists when the dispatcher starts), so it stays empty here.
}

// OutboxTTLDur is the parsed OutboxTTL. validate() guarantees it parses; this falls
// back to 2h only if called on an unvalidated/empty config.
func (c *Config) OutboxTTLDur() time.Duration {
	if c.OutboxTTL == "" {
		return 2 * time.Hour
	}
	d, err := time.ParseDuration(c.OutboxTTL)
	if err != nil {
		return 2 * time.Hour
	}
	return d
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
	if c.MaxIncomingMB < 1 {
		return fmt.Errorf("max_incoming_mb must be >= 1, got %d", c.MaxIncomingMB)
	}
	if _, err := time.ParseDuration(c.OutboxTTL); err != nil {
		return fmt.Errorf("outbox_ttl %q is not a valid duration (e.g. \"2h\", \"30m\", \"0\"): %w", c.OutboxTTL, err)
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
	if c.Workdir != "" && c.RuntimeBase != "" {
		return fmt.Errorf("runtime_base is meaningless with workdir: the responder cwd is the fixed $workdir/project, never ephemeral")
	}
	if c.UploadCommand != "" {
		// Fail fast on a misconfigured uploader rather than at the first big file.
		if _, err := os.Stat(c.UploadCommand); err != nil {
			return fmt.Errorf("upload_command %s: %w", c.UploadCommand, err)
		}
		if c.UploadThresholdMB < 1 {
			return fmt.Errorf("upload_threshold_mb must be >= 1, got %d", c.UploadThresholdMB)
		}
		if c.UploadMaxMB != 0 && c.UploadMaxMB < c.UploadThresholdMB {
			return fmt.Errorf("upload_max_mb (%d) must be >= upload_threshold_mb (%d)", c.UploadMaxMB, c.UploadThresholdMB)
		}
	}
	// An explicit transcript_dir must not sit under the responder workspace, which is
	// reset/wiped — the store must be durable (best-effort: the ephemeral-cwd case is
	// unknowable at load time and relies on the safe default instead).
	if c.Transcripts && c.TranscriptDir != "" {
		unsafe := []string{c.Project}
		if c.Workdir != "" {
			unsafe = append(unsafe, filepath.Join(c.Workdir, "project"))
		}
		for _, u := range unsafe {
			if u != "" && (c.TranscriptDir == u || strings.HasPrefix(c.TranscriptDir, u+string(os.PathSeparator))) {
				return fmt.Errorf("transcript_dir %q is under %q — it would be wiped with the responder workspace; put it elsewhere", c.TranscriptDir, u)
			}
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

// TranscriptRoot is the store root when the feature is on, else "". It defaults to
// <SessionDir>/transcripts (outside the outbox, so it survives the session-TTL wipe
// and restarts, per the design); transcript_dir overrides it.
func (c *Config) TranscriptRoot() string {
	if !c.Transcripts {
		return ""
	}
	if c.TranscriptDir != "" {
		return c.TranscriptDir
	}
	return filepath.Join(c.SessionDir(), "transcripts")
}

// OwnerReadsAllTranscripts reports whether the owner's responder reads the whole
// transcripts root (default true; validate/applyDefaults guarantee non-nil, but a
// nil is treated as the default here too).
func (c *Config) OwnerReadsAllTranscripts() bool {
	return c.OwnerReadsAll == nil || *c.OwnerReadsAll
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

// hookLogFile is where the PreToolUse hook appends its per-call log when --debug is
// on, else "" (off). It lives under StateDir (durable, dispatcher-owned) because
// Claude Code does not surface a hook's stderr to the dispatcher log.
func (c *Config) hookLogFile() string {
	if !c.Debug {
		return ""
	}
	return filepath.Join(c.StateDir, "pretooluse.log")
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
