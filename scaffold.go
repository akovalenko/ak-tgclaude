package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	DenyRead   []string `json:"denyRead,omitempty"`
	AllowRead  []string `json:"allowRead,omitempty"`
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
// Note: WRITE access to a specific outbox is added per invocation via
// buildInvocationSettings, so concurrent responders are each confined to their
// own. OutboxRoot here is used only to DENY read of the whole outbox area (a
// responder cannot read another chat's pending reply); each invocation carves
// read of just its own outbox back via the same overlay.
type scaffoldParams struct {
	CacheDir       string   // isolated Go caches root
	OutboxRoot     string   // parent of per-invocation outboxes (deny-read as a group)
	TranscriptRoot string   // transcript store root: deny-read the whole root; per-invocation allowRead carves the scope back
	UsageLogOn     bool     // usage-log feature on: materialize the tg-usage skill (available, NOT preloaded). The PATH stays out of static settings — access is a per-invocation allow/deny, see buildInvocationSettings
	TokenFile      string   // config file holding the token; "" if token came via --bot-token
	HookBinary     string   // default "ak-tgclaude"
	DenyEnvVars    []string // secrets to unset in the sandbox
	NetworkDomains []string // EXTRA egress domains (allow_domains), added to the always-present Go-build defaults
	UploadNote     string   // tg-emit {{UPLOAD_NOTE}} capability paragraph; empty => the large-file fallback is off
	Project        string   // knowledge root; substituted for {{PROJECT}} in agent/skill templates
	WireSkills     []string // operator skill template DIRECTORIES to materialize + preload
	AddSkills      []string // generic skill DIRECTORIES copied verbatim, NOT preloaded (on-demand)
	AddAgents      []string // generic agent .md FILES copied verbatim, NOT preloaded (on-demand)
	DenyRead       []string // operator paths denied at BOTH layers (Read-tool hook + sandbox Bash)
	Tools          []string // EXTRA tools granted to the responder (into tools: frontmatter AND --allowedTools)
	BangBug        bool     // pass --bang-bug to the hook (deny sandboxed Bash with the corrupted `\!`)
	HookLogFile    string   // pass --log-file to the hook (append every PreToolUse call here; "" => off; set under --debug)
}

// scaffoldParams derives the materializeScaffold inputs this config implies.
// It is the SINGLE builder behind both call sites — the dispatcher's startup
// and the scaffold subcommand — so the two can never drift field-by-field (a
// knob added to one but not the other). cacheDir and outboxRoot are the
// caller's runtime dirs, the only inputs that differ between the two.
func (c *Config) scaffoldParams(cacheDir, outboxRoot string) scaffoldParams {
	return scaffoldParams{
		CacheDir:       cacheDir,
		OutboxRoot:     outboxRoot,
		TranscriptRoot: c.TranscriptRoot(),
		UsageLogOn:     c.UsageLog != "",
		TokenFile:      c.ConfigPath,
		Project:        c.Project,
		WireSkills:     c.WireSkills,
		AddSkills:      c.AddSkills,
		AddAgents:      c.AddAgents,
		DenyRead:       c.DenyRead,
		Tools:          c.Tools,
		DenyEnvVars:    c.DenyEnvs,
		NetworkDomains: c.AllowDomains,
		UploadNote:     uploadNote(c.UploadCommand, c.UploadThresholdMB, c.UploadMaxMB),
		HookBinary:     selfExePath(),
		BangBug:        c.BangBug,
		HookLogFile:    c.hookLogFile(),
	}
}

// defaultDenyEnvVars are the ambient secrets scrubbed from the responder's
// sandboxed shell (its own model calls resolve the key before this bites).
var defaultDenyEnvVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// defaultNetworkDomains is the egress the responder needs to build Go code.
var defaultNetworkDomains = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}

// dedupStrings returns in with duplicates removed, order preserved.
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// combineTools merges the always-present base tools (the tg send tools) with the
// operator's extra tools (config `tools` / --tool), dropping blanks and duplicates
// (order-preserving). This ONE list is the single source for both grants:
// --allowedTools takes it verbatim (permission, so a scoped spec like
// WebFetch(domain:*.github.com) keeps its scope), while the agent's `tools:`
// frontmatter takes it through frontmatterTools (availability, keyed by bare name).
// Same source, deterministic transforms — so the two grants never drift.
func combineTools(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]bool, len(base)+len(extra))
	for _, t := range append(append([]string{}, base...), extra...) {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// baseToolName returns the availability name of a tool spec: the part before the
// first "(" — a permission scope such as WebFetch(domain:*.github.com) or
// Bash(git *) — trimmed. A bare name or an MCP pattern (no parens, e.g.
// mcp__tg__send_message) is returned unchanged. The agent's `tools:` frontmatter
// is an availability gate keyed by tool NAME, so a scoped spec must be reduced to
// its name there; the scope rides only on --allowedTools (the permission gate).
func baseToolName(spec string) string {
	if i := strings.IndexByte(spec, '('); i >= 0 {
		return strings.TrimSpace(spec[:i])
	}
	return strings.TrimSpace(spec)
}

// frontmatterTools maps a combined tool list to the availability names for the
// agent's `tools:` frontmatter: each entry reduced to its baseToolName and
// de-duplicated (order-preserving), blanks dropped. So WebFetch(domain:a) and
// WebFetch(domain:b) collapse to a single WebFetch in the frontmatter, while the
// full scoped specs stay verbatim on --allowedTools. Also keeps a literal "*"
// (e.g. an mcp__x__* pattern) out of the frontmatter unless the operator authored
// it as a bare name — a scope's "*" never leaks into the YAML tools: line.
func frontmatterTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		n := baseToolName(t)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// hostSecretCredFiles are host credential paths the sandboxed shell must never
// read: SSH private keys and Claude Code's own auth token. `~/` is expanded by
// the sandbox to the responder's home. Denying them does NOT break the
// responder's own `claude -p` auth — the parent process reads its credentials
// unsandboxed; only the Bash tools it spawns are confined (so a prompt-injected
// `cat ~/.ssh/id_rsa` is masked). Unconditional (independent of --config).
var hostSecretCredFiles = []string{
	"~/.ssh",
	"~/.claude/.credentials.json",
}

// hostSecretDenyRead are sensitive-but-not-credential host paths hidden from the
// sandboxed shell (so they go in filesystem.denyRead, not the credentials
// block): Claude Code's cross-session prompt/command history and the transcripts
// of the operator's other sessions (which may quote secrets from that work).
var hostSecretDenyRead = []string{
	"~/.claude/history.jsonl",
	"~/.claude/projects",
}

// goCacheEnv is the isolated Go cache environment for the responder. It is both
// written into settings.json and injected into the `claude -p` process env — the
// latter is what actually reaches the sandboxed `go` (a project settings-file
// `env` block does not propagate to tools under --setting-sources project; the
// sandbox does not strip the process env, so inheritance is the reliable path).
func goCacheEnv(cacheDir string) map[string]string {
	return map[string]string{
		"GOCACHE":             filepath.Join(cacheDir, "go-build"),
		"GOMODCACHE":          filepath.Join(cacheDir, "go-mod"),
		"GOLANGCI_LINT_CACHE": filepath.Join(cacheDir, "golangci-lint"),
		"GOPATH":              filepath.Join(cacheDir, "gopath"),
	}
}

// selfExePath is the absolute path of the running binary, or "" if it cannot be
// determined. The responder's PreToolUse hook is pinned to this path (baked into
// the generated settings.json) so the security-critical token guard runs the
// exact binary that wrote the settings, not whatever `ak-tgclaude` PATH happens
// to resolve at hook-fire time. On "" the caller falls back to the bare name.
func selfExePath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return ""
}

// buildSettings assembles the responder's .claude/settings.json.
func buildSettings(p scaffoldParams) *claudeSettings {
	if p.HookBinary == "" {
		// os.Executable() failed upstream (rare): fall back to the bare name, which
		// resolves via PATH. The dispatcher's startup check keeps that PATH entry
		// pointing at this binary.
		p.HookBinary = "ak-tgclaude"
	}
	// The Go-build defaults are ALWAYS present; operator NetworkDomains (allow_domains)
	// are ADDITIVE (mirroring DenyEnvVars on the deny side), de-duplicated, defaults
	// first — so a bot always builds Go out of the box and extra egress rides on top.
	p.NetworkDomains = dedupStrings(append(append([]string{}, defaultNetworkDomains...), p.NetworkDomains...))

	// The built-in secrets are ALWAYS scrubbed; operator DenyEnvVars are ADDITIVE
	// (a naive replace would drop the ANTHROPIC keys). De-duplicated, order kept.
	denyEnv := dedupStrings(append(append([]string{}, defaultDenyEnvVars...), p.DenyEnvVars...))
	envVars := make([]credEnv, 0, len(denyEnv))
	for _, name := range denyEnv {
		envVars = append(envVars, credEnv{Name: name, Mode: "deny"})
	}

	// Bash-layer read denies. The Read TOOL is already confined to the project by
	// the PreToolUse hook; this closes the Bash path (`cat`/`grep`) for host
	// secrets, operator-configured paths, and sibling outboxes. Host secrets first,
	// then operator --deny-read, then this run's outbox area (own outbox carved
	// back per invocation).
	denyReadFS := append([]string{}, hostSecretDenyRead...)
	denyReadFS = append(denyReadFS, p.DenyRead...)
	if p.OutboxRoot != "" {
		denyReadFS = append(denyReadFS, p.OutboxRoot)
	}
	// Deny-read the whole transcript root at the Bash layer; each invocation carves
	// its own scope (a chat's subdir, or the whole root for the owner) back via the
	// per-invocation allowRead overlay — so one chat cannot grep another's history.
	if p.TranscriptRoot != "" {
		denyReadFS = append(denyReadFS, p.TranscriptRoot)
	}

	// credentials.files: SSH keys + Claude's auth token (always), plus the bot's
	// own config file when the token lives there.
	credFiles := make([]credFile, 0, len(hostSecretCredFiles)+1)
	for _, path := range hostSecretCredFiles {
		credFiles = append(credFiles, credFile{Path: path, Mode: "deny"})
	}
	if p.TokenFile != "" {
		credFiles = append(credFiles, credFile{Path: p.TokenFile, Mode: "deny"})
	}

	s := &claudeSettings{
		Env: goCacheEnv(p.CacheDir),
		Permissions: &permissionsCfg{
			// The file tools (Read/Edit/Write/NotebookEdit) are governed by the
			// PreToolUse hook (path-scoped: read the project, write the outbox/tmp),
			// so they are NOT listed here. Only the tools the hook defers are
			// allowed — the search/skill tools; Bash is auto-allowed when sandboxed.
			Allow: []string{"Grep", "Glob", "Skill"},
		},
		Sandbox: &sandboxCfg{
			Enabled:                  true,
			AutoAllowBashIfSandboxed: true,
			AllowUnsandboxedCommands: false,
			Network:                  &networkCfg{AllowedDomains: p.NetworkDomains},
			Filesystem: &filesystemCfg{
				AllowWrite: []string{p.CacheDir},
				DenyRead:   denyReadFS,
			},
			Credentials: &credentialsCfg{EnvVars: envVars, Files: credFiles},
		},
	}

	// The hook's --deny-read scopes the Read TOOL (checked before the project-read
	// allow); the same paths also went into filesystem.denyRead above (the Bash
	// path). Operator paths first, then the token file when it lives in a config.
	// HookBinary is quoted: pinned to an absolute path it may contain spaces, and
	// quoting the bare name is harmless (still PATH-resolved).
	hookCmd := shellQuote(p.HookBinary) + " hook pretooluse"
	for _, d := range p.DenyRead {
		hookCmd += " --deny-read " + shellQuote(d)
	}
	if p.TokenFile != "" {
		hookCmd += " --deny-read " + shellQuote(p.TokenFile)
	}
	if p.BangBug {
		hookCmd += " --bang-bug"
	}
	if p.HookLogFile != "" {
		hookCmd += " --log-file " + shellQuote(p.HookLogFile)
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

// resetDirContents removes every entry inside dir without removing dir itself, so
// the caller can regenerate the contents from canon while preserving the dir's
// identity. Trust in ~/.claude.json is keyed by PATH, so a static workdir/project
// keeps its trust across a contents reset — but a remove+recreate of the dir would
// lose it. This is the "pure function of canon" primitive: on every start the
// workdir/project is reset, then materializeScaffold regenerates it, so a removed
// wire-skill or stale scaffold file never lingers.
func resetDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("resetting %s: %w", dir, err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("resetting %s: removing %s: %w", dir, p, err)
		}
	}
	return nil
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
	transcriptsOn := p.TranscriptRoot != ""
	if err := materializeSkills(claudeDir, p.Project, p.UploadNote, transcriptsOn, p.UsageLogOn); err != nil {
		return err
	}
	// Wire operator skill templates into the scaffold (materialized + {{PROJECT}}
	// substituted), then preload them into the built-in agent. On-demand skill
	// loading is not guaranteed (only the description is in context until the model
	// invokes it), so preloading via the agent's `skills:` is the reliable path for
	// a single-domain bot.
	wired, err := wireSkills(claudeDir, p.Project, p.WireSkills)
	if err != nil {
		return err
	}
	// Generic skills/agents are copied verbatim (no {{PROJECT}}) and NOT preloaded:
	// they live in the scaffold for on-demand use — the responder sees their
	// descriptions and pulls one in via the Skill tool / subagent delegation.
	if err := addSkills(claudeDir, p.AddSkills); err != nil {
		return err
	}
	if err := addAgents(claudeDir, p.AddAgents); err != nil {
		return err
	}
	// Preload tg-recall into the agent alongside operator wire-skills, but only when
	// the transcript feature is on (its body is materialized above under the same
	// gate). Off => neither the skill nor the preload — "ни записи, ни скилла".
	//
	// tg-usage is deliberately NOT here: when the usage-log feature is on its body is
	// materialized (available on-demand via the Skill tool, like a generic addSkill),
	// but it is NOT preloaded into the frontmatter — it is owner-only and rarely
	// needed, so it should not weigh on every ordinary user's turn.
	preload := append([]string(nil), wired...)
	if transcriptsOn {
		preload = append(preload, "tg-recall")
	}
	return materializeAgent(claudeDir, p.Project, preload, p.Tools)
}

// projectPlaceholder is replaced with the project path when an agent or skill
// template is materialized: the Read/Grep tools do not shell-expand $VARS in
// their path arguments, so a wired skill hard-codes {{PROJECT}}/notes/… and gets
// a literal absolute path. Absent placeholder => a no-op, so ordinary skills and
// the built-in assets pass through unchanged.
const projectPlaceholder = "{{PROJECT}}"

// mcpToolsPlaceholder is replaced in the responder agent's `tools:` frontmatter
// with the dispatcher's MCP send tools when the agent is materialized. The MCP
// tool names are a property of the invocation — they gate on the same mcpTools
// slice that builds --allowedTools (see mcp.go) — not of the authored agent, so
// the template carries only this marker and the names live in exactly one place.
const mcpToolsPlaceholder = "{{MCP_TOOLS}}"

// injectMCPTools replaces the {{MCP_TOOLS}} marker in an agent template with the
// MCP send tools appended to its `tools:` frontmatter list. The expansion carries
// its OWN leading ", " separator, and is empty when tools is empty — so the
// authored `tools: …Skill{{MCP_TOOLS}}` yields a comma-clean list either way (no
// dangling comma when there are no MCP tools). Data without the marker is returned
// unchanged.
func injectMCPTools(data []byte, tools []string) []byte {
	clause := ""
	if len(tools) > 0 {
		clause = ", " + strings.Join(tools, ", ")
	}
	return []byte(strings.ReplaceAll(string(data), mcpToolsPlaceholder, clause))
}

// uploadNotePlaceholder is replaced in the tg-emit skill with the large-file
// capability paragraph (uploadNote) when the fallback is configured, and with
// nothing when it is off — so the shipped skill stays generic and the operator's
// threshold/max numbers live in exactly one place (config).
const uploadNotePlaceholder = "{{UPLOAD_NOTE}}"

// injectUploadNote replaces the {{UPLOAD_NOTE}} marker with note (which carries its
// own trailing blank line when present). Data without the marker is unchanged.
func injectUploadNote(data []byte, note string) []byte {
	return []byte(strings.ReplaceAll(string(data), uploadNotePlaceholder, note))
}

// uploadNote is the tg-emit capability paragraph for the large-file fallback, or ""
// when it is off (no command) — the marker then vanishes. It carries its own
// trailing blank line so it sits cleanly between sections. The threshold/max come
// from config, keeping the numbers out of the shipped skill.
func uploadNote(command string, thresholdMB, maxMB int) string {
	if command == "" {
		return ""
	}
	note := fmt.Sprintf("**Large files.** A file over ~%d MB can't go out as a Telegram attachment directly, "+
		"but this bot handles it: write the file to your outbox and call `mcp__tg__send_document` as usual — "+
		"anything over that limit is uploaded to cloud storage and delivered to the chat as a link.", thresholdMB)
	if maxMB > 0 {
		note += fmt.Sprintf(" You can send files up to ~%d MB this way; a file larger than that is rejected with an error.", maxMB)
	}
	// The uploaded name reaches an operator script, so keep it plain — letters,
	// digits, spaces, and . _ - are safe; shell metacharacters are refused.
	note += " Give the file a plain name (letters, digits, spaces, `. _ -`); a name with shell metacharacters is refused."
	return note + "\n\n"
}

// materializeFile writes data to dst (creating parent dirs) with mode,
// substituting the project placeholder. It is the single copy path for every
// agent/skill file, embedded or wired. An empty project leaves the placeholder
// untouched, so an unmaterialized {{PROJECT}} is visible rather than becoming a
// broken path. mode is owner-only by scaffold convention (0o600, or 0o700 for a
// bundled executable — see scaffoldFileMode); an explicit chmod after the write
// makes it deterministic regardless of umask and of a pre-existing dst.
func materializeFile(dst string, data []byte, project string, mode os.FileMode) error {
	if project != "" {
		data = []byte(strings.ReplaceAll(string(data), projectPlaceholder, project))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// scaffoldFileMode maps a source file's permissions onto the scaffold's
// owner-only convention while preserving executability: an executable source
// (any x bit) becomes 0o700, a plain file 0o600. The scaffold is single-user, so
// group/other bits are intentionally dropped; all we must carry across is
// whether a bundled file (e.g. selftest.sh) has to stay runnable.
func scaffoldFileMode(src os.FileMode) os.FileMode {
	if src&0o111 != 0 {
		return 0o700
	}
	return 0o600
}

// materializeSkills copies the embedded skills tree into <cwd>/.claude/skills,
// substituting the {{UPLOAD_NOTE}} marker (in tg-emit) with the large-file
// capability paragraph — empty when the fallback is off, so the marker vanishes.
func materializeSkills(claudeDir, project, uploadNote string, transcriptsOn, usageLogOn bool) error {
	return fs.WalkDir(scaffoldAssets, "assets/skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// tg-recall ships to the responder only when the transcript feature is on.
		if !transcriptsOn && (p == "assets/skills/tg-recall" || strings.HasPrefix(p, "assets/skills/tg-recall/")) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// tg-usage ships only when the usage-log feature is on (like tg-recall). Unlike
		// tg-recall it is NOT preloaded into the agent — just available on demand.
		if !usageLogOn && (p == "assets/skills/tg-usage" || strings.HasPrefix(p, "assets/skills/tg-usage/")) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
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
		data = injectUploadNote(data, uploadNote)
		// Embedded files carry no exec bit (embed.FS is always 0444), so 0o600.
		return materializeFile(dst, data, project, 0o600)
	})
}

// copySkillDirs copies each skill DIRECTORY (its basename is the skill name)
// into <claudeDir>/skills/<name> and returns the names. The whole tree is
// copied, so bundled resources (reference.md, scripts, selftest) come along and
// executable bits are preserved; a bare SKILL.md file is rejected — copying only
// it would silently drop the skill's siblings (least surprise). project is
// substituted for {{PROJECT}} in every file ("" copies verbatim); verb labels
// errors after the config knob ("wire skill" / "add skill").
func copySkillDirs(claudeDir, project string, paths []string, verb string) ([]string, error) {
	var names []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", verb, p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s %s: must be a skill DIRECTORY, not a file "+
				"(the whole skill tree is copied so bundled resources come along) — pass %s instead",
				verb, p, filepath.Dir(p))
		}
		name := filepath.Base(p)
		if err := copyTreeMaterialize(p, filepath.Join(claudeDir, "skills", name), project); err != nil {
			return nil, fmt.Errorf("%s %s: %w", verb, p, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// wireSkills materializes each operator skill template into
// <cwd>/.claude/skills/<name> (substituting {{PROJECT}}) and returns the skill
// names, so they can be preloaded into the built-in agent. The directory
// basename must match the skill's frontmatter `name:` for the preload reference
// to resolve.
func wireSkills(claudeDir, project string, paths []string) ([]string, error) {
	return copySkillDirs(claudeDir, project, paths, "wire skill")
}

// addSkills copies each GENERIC skill DIRECTORY verbatim into
// <cwd>/.claude/skills/<name> — no {{PROJECT}} substitution and no agent
// preload. The skill is left for on-demand use: its description sits in the
// responder's context (the skill "table of contents") and it invokes the skill
// via the Skill tool when relevant.
func addSkills(claudeDir string, paths []string) error {
	_, err := copySkillDirs(claudeDir, "", paths, "add skill")
	return err
}

// addAgents copies each GENERIC agent .md FILE verbatim into
// <cwd>/.claude/agents/<basename> — no substitution, no preload. Claude Code
// agents are single markdown files (no bundled subtree), so a directory is
// rejected. The copied agent becomes a subagent the responder may delegate to on
// demand.
func addAgents(claudeDir string, paths []string) error {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("add agent %s: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("add agent %s: must be an agent .md FILE, not a directory "+
				"(Claude Code agents are single markdown files)", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("add agent %s: %w", p, err)
		}
		dst := filepath.Join(claudeDir, "agents", filepath.Base(p))
		if err := materializeFile(dst, data, "", scaffoldFileMode(info.Mode())); err != nil {
			return fmt.Errorf("add agent %s: %w", p, err)
		}
	}
	return nil
}

// copyTreeMaterialize recursively copies srcDir into dstDir, substituting
// {{PROJECT}} in every file and preserving executability (a bundled selftest.sh
// stays runnable — see scaffoldFileMode). A nested .git (e.g. when the template
// lives in its own repo and the path points at the repo root) is skipped.
func copyTreeMaterialize(srcDir, dstDir, project string) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" && p != srcDir {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return materializeFile(dst, data, project, scaffoldFileMode(info.Mode()))
	})
}

// policyPlaceholder marks where a persona once sat in the base agent template. It
// is now emptied at materialize time — the persona is composed per-user and
// injected at spawn via --append-system-prompt — so the base carries only the
// invariant prose (project access, replying, machine boundaries).
const policyPlaceholder = "{{POLICY}}"

// defaultPolicy is the persona composed when none is configured: the scoped FAQ
// that declines off-topic.
const defaultPolicy = "normal"

// builtinPolicyOrder lists the persona fragments shipped in assets/policies, in
// catalog order — the single source of truth for the built-in set. It backs
// builtinPolicies (membership), the `--policy help` catalog, and the "built-in: …"
// hints in error messages, so a new fragment is added in exactly one place. The
// refusal-stance trio (normal/norefuse/strict) all carry `axis: refusal` in their
// frontmatter, so at most one may appear in a resolved persona (see
// checkAxisConflicts); introspect and outbox-rw are axis-less and purely additive.
var builtinPolicyOrder = []string{"normal", "norefuse", "strict", "introspect", "outbox-rw"}

// builtinPolicies is the membership set derived from builtinPolicyOrder. A --policy
// value that is not one of these is treated as a path to a custom fragment .md.
var builtinPolicies = func() map[string]bool {
	m := make(map[string]bool, len(builtinPolicyOrder))
	for _, p := range builtinPolicyOrder {
		m[p] = true
	}
	return m
}()

// policyIsPath reports whether a policy selector names a custom fragment FILE
// (rather than a built-in): anything containing a path separator or ending in .md.
func policyIsPath(policy string) bool {
	return strings.ContainsRune(policy, filepath.Separator) || strings.HasSuffix(policy, ".md")
}

// readPolicyRaw returns the raw bytes for a policy selector: a built-in name reads
// assets/policies/<name>.md from the embed; anything else is a path to a custom
// fragment file read from disk. Empty selects defaultPolicy. The bytes may carry a
// leading `axis:` frontmatter block — use parseFragment to split it off.
func readPolicyRaw(policy string) ([]byte, error) {
	if policy == "" {
		policy = defaultPolicy
	}
	if policyIsPath(policy) {
		data, err := os.ReadFile(policy)
		if err != nil {
			return nil, fmt.Errorf("reading custom policy %s: %w", policy, err)
		}
		return data, nil
	}
	if !builtinPolicies[policy] {
		return nil, fmt.Errorf("unknown policy %q (built-in: %s; or a path to a .md fragment)", policy, strings.Join(builtinPolicyOrder, ", "))
	}
	return scaffoldAssets.ReadFile("assets/policies/" + policy + ".md")
}

// parseFragment splits a policy fragment into its frontmatter fields (empty map if
// none) and its body with the frontmatter removed. Frontmatter is an OPT-IN leading
// `---` … `---` block of `key: value` lines; we read `axis` (the mutual-exclusion
// guard, so two fragments sharing a non-empty axis cannot co-exist in one resolved
// persona) and `summary` (the one-line gloss shown by `--policy help`). A fragment
// with no leading fence (or no closing fence) is all body with no fields, so the
// plain "just write an .md" case needs no ceremony. Parsed by hand (no YAML
// dependency): the block is a handful of `key: value` lines.
func parseFragment(data []byte) (fields map[string]string, body []byte) {
	fields = map[string]string{}
	rest, ok := strings.CutPrefix(string(data), "---\n")
	if !ok {
		return fields, data
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fields, data // no closing fence — treat the whole thing as body
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	// Body is everything past the closing fence line.
	return fields, []byte(strings.TrimPrefix(rest[end+len("\n---"):], "\n"))
}

// policyAxis returns the axis a policy selector declares (empty if none).
func policyAxis(policy string) (string, error) {
	raw, err := readPolicyRaw(policy)
	if err != nil {
		return "", err
	}
	fields, _ := parseFragment(raw)
	return fields["axis"], nil
}

// policySummary returns the one-line `summary:` a policy selector declares (empty if
// none). It backs the `--policy help` catalog.
func policySummary(policy string) (string, error) {
	raw, err := readPolicyRaw(policy)
	if err != nil {
		return "", err
	}
	fields, _ := parseFragment(raw)
	return fields["summary"], nil
}

// printPolicyCatalog writes the built-in policy catalog — each name aligned with its
// `summary:` gloss, in builtinPolicyOrder — followed by a note on custom fragments and
// composition. It backs `--policy help`. The summaries come from the embed, so a read
// error is unexpected but surfaced rather than swallowed.
func printPolicyCatalog(w io.Writer) error {
	width := 0
	for _, p := range builtinPolicyOrder {
		if len(p) > width {
			width = len(p)
		}
	}
	fmt.Fprintln(w, "built-in policies (persona fragments; the default is `normal`):")
	fmt.Fprintln(w)
	for _, p := range builtinPolicyOrder {
		summary, err := policySummary(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, p, summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A --policy value may also be a path to your own .md fragment. --policy is")
	fmt.Fprintln(w, "repeatable and additive: entries merge in order into one persona (the refusal")
	fmt.Fprintln(w, "trio normal/norefuse/strict are mutually exclusive; the rest are additive).")
	return nil
}

// loadPolicies merges the persona-fragment BODIES for a list of selectors into a
// single fragment: each is read, its frontmatter stripped, trimmed of surrounding
// blank lines, and joined in order with a blank line between them, so several
// stances (built-in names and/or custom paths) layer into one persona. An empty
// list selects defaultPolicy — the single-selector case is just one element.
func loadPolicies(policies []string) ([]byte, error) {
	if len(policies) == 0 {
		policies = []string{defaultPolicy}
	}
	parts := make([]string, 0, len(policies))
	for _, p := range policies {
		raw, err := readPolicyRaw(p)
		if err != nil {
			return nil, err
		}
		_, body := parseFragment(raw)
		parts = append(parts, strings.TrimSpace(string(body)))
	}
	return []byte(strings.Join(parts, "\n\n")), nil
}

// checkAxisConflicts reports an error if two selectors in the list declare the
// same non-empty axis — the opt-in mutual-exclusion guard. It runs at config load
// over the default set and over each per-user override list, so a contradictory
// pairing (e.g. norefuse + strict) fails fast at startup, not mid-run.
func checkAxisConflicts(policies []string) error {
	seen := make(map[string]string, len(policies))
	for _, p := range policies {
		axis, err := policyAxis(p)
		if err != nil {
			return err
		}
		if axis == "" {
			continue
		}
		if prev, ok := seen[axis]; ok {
			return fmt.Errorf("policies %q and %q both declare axis %q — only one per axis", prev, p, axis)
		}
		seen[axis] = p
	}
	return nil
}

// withDefaultStance ensures the resolved persona has a fragment on defaultPolicy's
// axis — the "refusal" axis (normal/norefuse/strict). Axis-less fragments (introspect,
// outbox-rw, or a plain custom .md) are MODIFIERS meant to layer on top of a base
// stance; a list of only those — e.g. a lone `--policy ./my-rw.md` — would otherwise
// leave the agent with no base FAQ stance at all. When no fragment claims that axis,
// defaultPolicy (normal) is prepended as the base, generalizing the empty-list
// fallback. A custom fragment can occupy the slot itself by declaring `axis: refusal`,
// which suppresses the injection (the escape hatch for a deliberately base-less
// persona). An empty list is returned unchanged — loadPolicies maps it to
// defaultPolicy on its own.
func withDefaultStance(policies []string) ([]string, error) {
	if len(policies) == 0 {
		return policies, nil
	}
	base, err := policyAxis(defaultPolicy)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return policies, nil // defaultPolicy declares no axis — nothing to floor
	}
	for _, p := range policies {
		axis, err := policyAxis(p)
		if err != nil {
			return nil, err
		}
		if axis == base {
			return policies, nil // the axis is already occupied
		}
	}
	return append([]string{defaultPolicy}, policies...), nil
}

// resolveEffectivePolicies layers a per-user override list on top of the default
// list along axes: an override fragment that declares an axis EVICTS the default
// fragment on that same axis (replacing it in place); an axis-less override (or one
// whose axis no default carries) is appended. So a default of {strict, rw} with a
// user override of {norefuse} yields {norefuse, rw} — norefuse displaces strict on
// the refusal axis, rw is untouched. The override list is assumed already free of
// internal axis conflicts (checked at load).
func resolveEffectivePolicies(base, override []string) ([]string, error) {
	result := append([]string(nil), base...)
	axisAt := make(map[string]int) // axis -> index in result
	for i, p := range result {
		axis, err := policyAxis(p)
		if err != nil {
			return nil, err
		}
		if axis != "" {
			axisAt[axis] = i
		}
	}
	for _, o := range override {
		axis, err := policyAxis(o)
		if err != nil {
			return nil, err
		}
		if i, ok := axisAt[axis]; axis != "" && ok {
			result[i] = o // evict the default fragment on this axis
			continue
		}
		result = append(result, o)
		if axis != "" {
			axisAt[axis] = len(result) - 1
		}
	}
	return result, nil
}

// materializeAgent writes the responder agent into
// <cwd>/.claude/agents/faq-responder.md from the base template: the base carries
// the invariant mechanics (project access, replying, machine boundaries) and is
// persona-NEUTRAL — the persona is composed per-user and injected at spawn via
// --append-system-prompt (see the dispatcher's persona resolution and
// claudeResponder.buildArgs), so one shared agent file serves every chat. Machine guards
// (sandbox, token deny-read, per-invocation write, pinned route) hold regardless of
// persona, so a relaxed policy cannot exceed them. wiredSkills are appended to the
// agent's `skills:` frontmatter so their bodies are preloaded at startup, and the
// `tools:` line's {{MCP_TOOLS}} marker is expanded from the mcpTools source.
func materializeAgent(claudeDir string, project string, wiredSkills, extraTools []string) error {
	data, err := scaffoldAssets.ReadFile("assets/agents/faq-responder.md")
	if err != nil {
		return err
	}
	// Persona is not baked here (it rides --append-system-prompt at spawn); drop the
	// {{POLICY}} marker and collapse the blank hole it leaves.
	body := strings.ReplaceAll(string(data), policyPlaceholder, "")
	for strings.Contains(body, "\n\n\n") {
		body = strings.ReplaceAll(body, "\n\n\n", "\n\n")
	}
	data = []byte(body)
	data = appendAgentSkills(data, wiredSkills)
	// Expand {{MCP_TOOLS}} in the tools: frontmatter from the tg send tools plus any
	// operator extras (config `tools`/--tool), reduced to availability NAMES via
	// frontmatterTools (a scoped spec like WebFetch(domain:X) becomes bare WebFetch).
	// claudeResponder.buildArgs feeds --allowedTools from the SAME combineTools list but
	// verbatim (scope kept), so availability and permission never drift and the
	// authored template stays MCP-agnostic.
	data = injectMCPTools(data, frontmatterTools(combineTools(mcpTools, extraTools)))
	dst := filepath.Join(claudeDir, "agents", "faq-responder.md")
	return materializeFile(dst, data, project, 0o600)
}

// appendAgentSkills adds skill names to the inline `skills: [a, b]` list in an
// agent markdown's YAML frontmatter (order-preserving, de-duplicated), so wired
// skills are preloaded into the agent's context at startup. It handles the inline
// form the shipped agents use, inserting a `skills:` line before the closing
// frontmatter fence if none exists. Data without frontmatter is returned as-is.
func appendAgentSkills(data []byte, add []string) []byte {
	if len(add) == 0 {
		return data
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return data // no frontmatter
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return data // unterminated frontmatter
	}
	merge := func(existing []string) string {
		seen := map[string]bool{}
		var out []string
		for _, n := range append(existing, add...) {
			if n = strings.TrimSpace(n); n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
		return "skills: [" + strings.Join(out, ", ") + "]"
	}
	for i := 1; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "skills:") {
			var existing []string
			if l, r := strings.IndexByte(lines[i], '['), strings.LastIndexByte(lines[i], ']'); l >= 0 && r > l {
				existing = strings.Split(lines[i][l+1:r], ",")
			}
			lines[i] = merge(existing)
			return []byte(strings.Join(lines, "\n"))
		}
	}
	// No skills: line — insert one just before the closing fence.
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:end]...)
	out = append(out, merge(nil))
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, "\n"))
}

// buildInvocationSettings returns the per-invocation --settings JSON that scopes
// SANDBOXED-BASH access for exactly this invocation, merged on top of the static
// project settings:
//   - allowWrite — so `send`/`cp` can write only this outbox, not a sibling's;
//   - allowRead — carving this outbox back out of the static denyRead so
//     `send --file` can read its own body, while sibling outboxes stay masked;
//     also the transcript scope, and — for the OWNER — the usage-log file.
//   - denyRead — for NON-owners, the usage-log file (see below).
//
// Usage log: it appears in NO static setting. Each invocation gets EXACTLY ONE of
// allow/deny for it — the owner an allowRead, everyone else a denyRead — never
// both, so the (undocumented) allow-vs-deny precedence for an identical path never
// arises. Read is allow-by-default, so the per-invocation denyRead is what actually
// closes the file to non-owners; the owner's allowRead is explicit (and would carry
// the carve even if the base default ever changed). usageLog=="" => feature off,
// nothing emitted either way.
//
// The Write TOOL to the outbox is granted by the PreToolUse hook (path-scoped),
// not here. All inputs empty => "".
func buildInvocationSettings(outbox, transcriptScope, usageLog string, usageLogOwner bool) string {
	if outbox == "" && transcriptScope == "" && usageLog == "" {
		return ""
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite,omitempty"`
				AllowRead  []string `json:"allowRead,omitempty"`
				DenyRead   []string `json:"denyRead,omitempty"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if outbox != "" {
		s.Sandbox.Filesystem.AllowWrite = []string{outbox}
		s.Sandbox.Filesystem.AllowRead = []string{outbox}
	}
	if transcriptScope != "" {
		// Read-only: carve the chat's own transcript subdir (or, for the owner, the
		// whole root) back out of the static denyRead. Never AllowWrite.
		s.Sandbox.Filesystem.AllowRead = append(s.Sandbox.Filesystem.AllowRead, transcriptScope)
	}
	if usageLog != "" {
		// Exactly one of allow/deny — never both (see the doc comment). Read-only either
		// way; the usage log is never writable by the responder (the dispatcher owns it).
		if usageLogOwner {
			s.Sandbox.Filesystem.AllowRead = append(s.Sandbox.Filesystem.AllowRead, usageLog)
		} else {
			s.Sandbox.Filesystem.DenyRead = append(s.Sandbox.Filesystem.DenyRead, usageLog)
		}
	}
	b, _ := json.Marshal(&s)
	return string(b)
}

// shellQuote single-quotes a path for embedding in the hook command string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runScaffold materializes the responder's workdir/project WITHOUT running the
// dispatcher, so the operator can inspect the generated settings, tweak
// settings.local.json, and run `claude` there by hand to observe the sandbox. It
// materializes what the dispatcher regenerates on startup (minus the contents
// reset), so point --workdir at a throwaway dir to inspect without touching a live
// bot's project. Failures are returned for main to report and exit-code.
func runScaffold(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return usageError{err}
	}
	if cfg.Workdir == "" {
		return usageError{errors.New("--workdir is required (its project/ is materialized for inspection)")}
	}
	project := filepath.Join(cfg.Workdir, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		return err
	}
	outboxRoot := filepath.Join(project, "outbox")
	if err := os.MkdirAll(outboxRoot, 0o700); err != nil {
		return err
	}
	if err := materializeScaffold(project, cfg.scaffoldParams(filepath.Join(cfg.StateDir, "cache"), outboxRoot)); err != nil {
		return err
	}

	fmt.Printf("ak-tgclaude: scaffold materialized\n")
	fmt.Printf("  project:  %s\n", project)
	fmt.Printf("  settings: %s\n", filepath.Join(project, ".claude", "settings.json"))
	fmt.Printf("  policies: %s (default persona; injected at spawn)\n", strings.Join(cfg.Policies, " + "))
	if len(cfg.WireSkills) > 0 {
		fmt.Printf("  wired:    %s (preloaded into the agent)\n", strings.Join(cfg.WireSkills, ", "))
	}
	if len(cfg.AddSkills) > 0 {
		fmt.Printf("  added skills: %s (verbatim, on-demand — not preloaded)\n", strings.Join(cfg.AddSkills, ", "))
	}
	if len(cfg.AddAgents) > 0 {
		fmt.Printf("  added agents: %s (verbatim, on-demand)\n", strings.Join(cfg.AddAgents, ", "))
	}
	if len(cfg.Tools) > 0 {
		fmt.Printf("  extra tools: %s (into tools: frontmatter + --allowedTools)\n", strings.Join(cfg.Tools, ", "))
	}
	if len(cfg.DenyRead) > 0 {
		fmt.Printf("  deny-read: %s (Read-tool + sandboxed Bash)\n", strings.Join(cfg.DenyRead, ", "))
	}
	fmt.Printf("  outbox:   %s\n", outboxRoot)
	if cfg.ConfigPath == "" {
		fmt.Printf("  (no --config given: the token guard has no deny-read path)\n")
	}
	agentFlag := ""
	if cfg.Agent != "" {
		agentFlag = " --agent " + cfg.Agent
	}
	// This is a copy-pasteable shell command, so shell-quote the values that carry a
	// runtime path (the outbox, under workdir) or JSON with metacharacters (the
	// settings overlay): a space or an apostrophe in the workdir — both legal, since
	// only glob metacharacters are rejected — would otherwise break the paste. The
	// dispatcher's own invocation is argv, not a shell, so it needs none of this.
	fmt.Printf("\nrun claude there by hand to observe the sandbox (the --settings\n")
	fmt.Printf("overlay grants write to just this outbox, as the dispatcher does per invocation;\n")
	fmt.Printf("the MCP send tools are NOT wired here — they need the running dispatcher):\n")
	fmt.Printf("  cd %s\n", shellQuote(project))
	fmt.Printf("  AK_TGCLAUDE_OUTBOX=%s claude -p --setting-sources project --permission-mode dontAsk \\\n", shellQuote(outboxRoot))
	fmt.Printf("    --settings %s%s 'hello'\n", shellQuote(buildInvocationSettings(outboxRoot, "", "", false)), agentFlag)
	return nil
}
