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
	TokenFile      string   // config file holding the token; "" if token came via --bot-token
	HookBinary     string   // default "ak-tgclaude"
	DenyEnvVars    []string // secrets to unset in the sandbox
	NetworkDomains []string // sandbox egress allowlist
	NoRefuse       bool     // materialize the do-what-you're-asked agent variant
	Project        string   // knowledge root; substituted for {{PROJECT}} in agent/skill templates
	WireSkills     []string // operator skill template DIRECTORIES to materialize + preload
	AddSkills      []string // generic skill DIRECTORIES copied verbatim, NOT preloaded (on-demand)
	AddAgents      []string // generic agent .md FILES copied verbatim, NOT preloaded (on-demand)
	DenyRead       []string // operator paths denied at BOTH layers (Read-tool hook + sandbox Bash)
	BangBug        bool     // pass --bang-bug to the hook (deny sandboxed Bash with the corrupted `\!`)
}

// defaultDenyEnvVars are the ambient secrets scrubbed from the responder's
// sandboxed shell (its own model calls resolve the key before this bites).
var defaultDenyEnvVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// defaultNetworkDomains is the egress the responder needs to build Go code.
var defaultNetworkDomains = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}

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
	if err := materializeSkills(claudeDir, p.Project); err != nil {
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
	return materializeAgent(claudeDir, p.NoRefuse, p.Project, wired)
}

// projectPlaceholder is replaced with the project path when an agent or skill
// template is materialized: the Read/Grep tools do not shell-expand $VARS in
// their path arguments, so a wired skill hard-codes {{PROJECT}}/notes/… and gets
// a literal absolute path. Absent placeholder => a no-op, so ordinary skills and
// the built-in assets pass through unchanged.
const projectPlaceholder = "{{PROJECT}}"

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

// materializeSkills copies the embedded skills tree into <cwd>/.claude/skills.
func materializeSkills(claudeDir, project string) error {
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
		// Embedded files carry no exec bit (embed.FS is always 0444), so 0o600.
		return materializeFile(dst, data, project, 0o600)
	})
}

// wireSkills materializes each operator skill template into
// <cwd>/.claude/skills/<name> (substituting {{PROJECT}}) and returns the skill
// names, so they can be preloaded into the built-in agent. Each path must be a
// skill DIRECTORY (its basename is the skill name): the whole tree is copied, so
// bundled resources (reference.md, scripts, selftest) come along and executable
// bits are preserved. A bare SKILL.md file is rejected — copying only it would
// silently drop the skill's siblings (least surprise). The directory basename
// must match the skill's frontmatter `name:` for the preload reference to resolve.
func wireSkills(claudeDir, project string, paths []string) ([]string, error) {
	var names []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("wire skill %s: %w", p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("wire skill %s: must be a skill DIRECTORY, not a file "+
				"(the whole skill tree is copied so bundled resources come along) — pass %s instead",
				p, filepath.Dir(p))
		}
		name := filepath.Base(p)
		if err := copyTreeMaterialize(p, filepath.Join(claudeDir, "skills", name), project); err != nil {
			return nil, fmt.Errorf("wire skill %s: %w", p, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// addSkills copies each GENERIC skill DIRECTORY verbatim into
// <cwd>/.claude/skills/<name> — no {{PROJECT}} substitution (empty project) and
// no agent preload. The skill is left for on-demand use: its description sits in
// the responder's context (the skill "table of contents") and it invokes the
// skill via the Skill tool when relevant. Like wireSkills, a bare SKILL.md file is
// rejected — the whole tree is copied so bundled resources (and +x) come along.
func addSkills(claudeDir string, paths []string) error {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("add skill %s: %w", p, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("add skill %s: must be a skill DIRECTORY, not a file "+
				"(the whole skill tree is copied so bundled resources come along) — pass %s instead",
				p, filepath.Dir(p))
		}
		name := filepath.Base(p)
		if err := copyTreeMaterialize(p, filepath.Join(claudeDir, "skills", name), ""); err != nil {
			return fmt.Errorf("add skill %s: %w", p, err)
		}
	}
	return nil
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

// materializeAgent writes the chosen responder agent variant into
// <cwd>/.claude/agents/faq-responder.md. Both variants carry the same agent name
// (so --agent selection is unchanged); noRefuse swaps the persona from a scoped
// FAQ that declines off-topic to a do-what-you're-asked assistant. Machine
// guards (sandbox, token deny-read, per-invocation write, pinned route) hold
// either way, so the relaxed persona cannot exceed them. wiredSkills are appended
// to the agent's `skills:` frontmatter so their bodies are preloaded at startup.
func materializeAgent(claudeDir string, noRefuse bool, project string, wiredSkills []string) error {
	src := "assets/agents/faq-responder.md"
	if noRefuse {
		src = "assets/agents/faq-responder.norefuse.md"
	}
	data, err := scaffoldAssets.ReadFile(src)
	if err != nil {
		return err
	}
	data = appendAgentSkills(data, wiredSkills)
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
// SANDBOXED-BASH access to exactly this invocation's outbox, merged on top of the
// static project settings:
//   - allowWrite — so `send`/`cp` can write only this outbox, not a sibling's;
//   - allowRead — carving this outbox back out of the static denyRead so
//     `send --file` can read its own body, while sibling outboxes stay masked.
//
// The Write TOOL to the outbox is granted by the PreToolUse hook (path-scoped),
// not here. Empty outbox => "".
func buildInvocationSettings(outbox string) string {
	if outbox == "" {
		return ""
	}
	var s struct {
		Sandbox struct {
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
				AllowRead  []string `json:"allowRead"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	s.Sandbox.Filesystem.AllowWrite = []string{outbox}
	s.Sandbox.Filesystem.AllowRead = []string{outbox}
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
// bot's project.
func runScaffold(args []string) {
	cfg, err := parseConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(2)
	}
	if cfg.Workdir == "" {
		fmt.Fprintln(os.Stderr, "ak-tgclaude: scaffold: --workdir is required (its project/ is materialized for inspection)")
		os.Exit(2)
	}
	project := filepath.Join(cfg.Workdir, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}
	outboxRoot := filepath.Join(project, "outbox")
	if err := os.MkdirAll(outboxRoot, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}
	if err := materializeScaffold(project, scaffoldParams{
		CacheDir:   filepath.Join(cfg.StateDir, "cache"),
		OutboxRoot: outboxRoot,
		TokenFile:  cfg.ConfigPath,
		NoRefuse:   cfg.NoRefuse,
		Project:    cfg.Project,
		WireSkills: cfg.WireSkills,
		AddSkills:  cfg.AddSkills,
		AddAgents:  cfg.AddAgents,
		DenyRead:   cfg.DenyRead,
		HookBinary: selfExePath(),
		BangBug:    cfg.BangBug,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: scaffold: %v\n", err)
		os.Exit(1)
	}

	agentVariant := "faq (declines off-topic)"
	if cfg.NoRefuse {
		agentVariant = "norefuse (do-what-you're-asked)"
	}
	fmt.Printf("ak-tgclaude: scaffold materialized\n")
	fmt.Printf("  project:  %s\n", project)
	fmt.Printf("  settings: %s\n", filepath.Join(project, ".claude", "settings.json"))
	fmt.Printf("  agent:    %s\n", agentVariant)
	if len(cfg.WireSkills) > 0 {
		fmt.Printf("  wired:    %s (preloaded into the agent)\n", strings.Join(cfg.WireSkills, ", "))
	}
	if len(cfg.AddSkills) > 0 {
		fmt.Printf("  added skills: %s (verbatim, on-demand — not preloaded)\n", strings.Join(cfg.AddSkills, ", "))
	}
	if len(cfg.AddAgents) > 0 {
		fmt.Printf("  added agents: %s (verbatim, on-demand)\n", strings.Join(cfg.AddAgents, ", "))
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
	fmt.Printf("\nrun claude there by hand to observe the sandbox (the --settings\n")
	fmt.Printf("overlay grants write to just this outbox, as the dispatcher does per invocation):\n")
	fmt.Printf("  cd %s\n", project)
	fmt.Printf("  AK_TGCLAUDE_OUTBOX=%s claude -p --setting-sources project --permission-mode dontAsk \\\n", outboxRoot)
	fmt.Printf("    --settings '%s'%s 'hello'\n", buildInvocationSettings(outboxRoot), agentFlag)
}
