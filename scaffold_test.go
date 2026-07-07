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
	// No allow list — tools are granted via the hook (file tools), the sandbox
	// (Bash), and --allowedTools (MCP send + Skill); a populated allow list only
	// trips the untrusted-workspace warning. Only permissions.deny is set: the
	// non-timeout-able secret backstop (tested in full below).
	if len(s.Permissions.Allow) != 0 {
		t.Errorf("static settings should carry no permissions.allow (granted via --allowedTools), got %v", s.Permissions.Allow)
	}
	if !contains(s.Permissions.Deny, "Read(//cfg/bot.toml)") {
		t.Errorf("permissions.deny should back-stop the token file, got %v", s.Permissions.Deny)
	}
	// denyRead now carries only the sibling-outbox root (Bash isn't hook-scoped);
	// own outbox is carved back per invocation. Host secrets moved to the
	// whole-directory ~/.claude / ~/.ssh deny in credentials.files.
	if got := s.Sandbox.Filesystem.DenyRead; len(got) != 1 || got[0] != "/run/out" {
		t.Errorf("sandbox denyRead = %v, want [/run/out]", got)
	}
	// credentials.files: the host secret DIRECTORIES (~/.ssh, ~/.claude) always,
	// then the bot's config file since the token lives there.
	files := s.Sandbox.Credentials.Files
	if len(files) != 3 || files[0].Path != "~/.ssh" ||
		files[1].Path != "~/.claude" || files[2].Path != "/cfg/bot.toml" {
		t.Errorf("credentials.files = %+v", files)
	}
	for _, f := range files {
		if f.Mode != "deny" {
			t.Errorf("credentials.files entry not deny: %+v", f)
		}
	}
	if len(s.Sandbox.Credentials.EnvVars) != 3 || s.Sandbox.Credentials.EnvVars[0].Mode != "deny" {
		t.Errorf("credentials.envVars = %+v", s.Sandbox.Credentials.EnvVars)
	}

	// Hook command carries only behavioral flags — the file-tool policy (read/write/
	// deny roots) rides AK_TGCLAUDE_FILE_POLICY at spawn, not the command. The binary
	// is quoted (bare name here, since HookBinary is unset => the default).
	cmd := s.Hooks.PreToolUse[0].Hooks[0].Command
	if !strings.HasPrefix(cmd, "'ak-tgclaude' hook pretooluse") || strings.Contains(cmd, "--deny-read") {
		t.Errorf("hook command = %q", cmd)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestBuildSettingsPermissionDenyBackstop(t *testing.T) {
	// The permissions.deny backstop mirrors the hook's secret set — host secrets,
	// the token file, AND operator deny_reads (an operator's ~/.aws must be guarded
	// exactly like a built-in) — as //-anchored Read rules that also cover Grep/Glob.
	s := buildSettings(scaffoldParams{
		CacheDir:  "/c",
		TokenFile: "/cfg/bot.toml",
		DenyRead:  []string{"/home/op/.aws", "/mnt/secrets/prod.env"},
	})
	deny := s.Permissions.Deny
	for _, want := range []string{
		"Read(//cfg/bot.toml)",         // token
		"Read(//home/op/.aws)",         // operator secret — equal to built-ins
		"Read(//mnt/secrets/prod.env)", // operator secret
	} {
		if !contains(deny, want) {
			t.Errorf("permissions.deny missing %q, got %v", want, deny)
		}
	}
	// A host secret is backstopped too (path is home-relative, so match by suffix).
	sshBackstopped := false
	for _, d := range deny {
		if strings.HasSuffix(d, ".ssh)") {
			sshBackstopped = true
		}
	}
	if !sshBackstopped {
		t.Errorf("permissions.deny should back-stop the host ssh key, got %v", deny)
	}
	// No rule carries a `/**` subtree suffix: a directory deny already covers its
	// contents, and `/**` on a file path corrupts the sandbox mask (bwrap aborts and
	// the responder's sandboxed shell never starts). See permissionDenyRules.
	for _, d := range deny {
		if strings.HasSuffix(d, "/**)") {
			t.Errorf("deny rule must not carry a /** subtree suffix: %q", d)
		}
	}
	// Every rule is //-anchored (absolute) — a single-/ rule would resolve relative
	// to the settings file and protect the wrong path.
	for _, d := range deny {
		if !strings.HasPrefix(d, "Read(//") {
			t.Errorf("deny rule not //-anchored (absolute): %q", d)
		}
	}
}

// permissionDenyRules denies each secret under BOTH its lexical and its
// symlink-resolved spelling, so a starved fail-open hook cannot be dodged by a
// symlinked deny path. A dangling link (no target) yields only the lexical rule; a
// plain real directory yields exactly one rule (no spurious duplicate).
func TestPermissionDenyRulesDoubleDenySymlink(t *testing.T) {
	// Resolve the temp base first: if TMPDIR itself has a symlinked component, an
	// unresolved base would make even a plain dir emit a second (resolved) rule and
	// break the exact-count assertions below.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realSecret := filepath.Join(base, "real")
	if err := os.Mkdir(realSecret, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realSecret, link); err != nil {
		t.Fatal(err)
	}

	// A symlinked deny path: both the link and its resolved target are denied.
	rules := permissionDenyRules([]string{link})
	if !contains(rules, "Read(/"+link+")") {
		t.Errorf("want lexical deny for the link %q, got %v", link, rules)
	}
	if !contains(rules, "Read(/"+realSecret+")") {
		t.Errorf("want resolved deny for the target %q, got %v", realSecret, rules)
	}

	// A dangling link resolves to nothing: only the lexical rule survives.
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nope"), dangling); err != nil {
		t.Fatal(err)
	}
	if got := permissionDenyRules([]string{dangling}); len(got) != 1 || got[0] != "Read(/"+dangling+")" {
		t.Errorf("dangling link: want only the lexical rule, got %v", got)
	}

	// A plain real directory (no symlink in the path): exactly one rule, no duplicate.
	if got := permissionDenyRules([]string{realSecret}); len(got) != 1 || got[0] != "Read(/"+realSecret+")" {
		t.Errorf("plain dir: want exactly one rule, got %v", got)
	}
}

func TestBuildSettingsDenyWrite(t *testing.T) {
	// No ProjectDir (a bare buildSettings, e.g. a unit test) => no denyWrite, so the
	// existing shape tests keep their expectations.
	if got := buildSettings(scaffoldParams{CacheDir: "/c"}).Sandbox.Filesystem.DenyWrite; got != nil {
		t.Errorf("no ProjectDir should yield no denyWrite, got %v", got)
	}
	// With a ProjectDir the cwd is write-denied (covers its .claude subdir); allowWrite
	// stays just the cache. The outbox is granted per invocation and lives OUTSIDE cwd,
	// so it is never an allow nested inside this deny.
	s := buildSettings(scaffoldParams{CacheDir: "/c", ProjectDir: "/wd/project"})
	if got := s.Sandbox.Filesystem.DenyWrite; len(got) != 1 || got[0] != "/wd/project" {
		t.Errorf("denyWrite = %v, want [/wd/project]", got)
	}
	if got := s.Sandbox.Filesystem.AllowWrite; len(got) != 1 || got[0] != "/c" {
		t.Errorf("allowWrite should stay just the cache, got %v", got)
	}
}

func TestMaterializeScaffoldDeniesCwd(t *testing.T) {
	// materializeScaffold stamps the cwd into denyWrite from its own arg, so the
	// written settings.json write-denies the responder's launch dir.
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: filepath.Join(t.TempDir(), "cache")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s claudeSettings
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if got := s.Sandbox.Filesystem.DenyWrite; len(got) != 1 || got[0] != cwd {
		t.Errorf("materialized denyWrite = %v, want [%s] (the cwd)", got, cwd)
	}
}

func TestMaterializeScaffoldSandboxMaskStubs(t *testing.T) {
	// The bwrap bind-mount targets must pre-exist (as the right kind) because cwd is
	// denyWrite'd — otherwise a sandboxed Bash EROFSes trying to create them.
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: filepath.Join(t.TempDir(), "cache")}); err != nil {
		t.Fatal(err)
	}
	for _, f := range sandboxMaskFiles {
		fi, err := os.Stat(filepath.Join(cwd, f))
		if err != nil {
			t.Errorf("mask stub %s missing: %v", f, err)
		} else if fi.IsDir() {
			t.Errorf("mask stub %s should be a file, got a dir", f)
		}
	}
	for _, d := range sandboxMaskDirs {
		fi, err := os.Stat(filepath.Join(cwd, d))
		if err != nil {
			t.Errorf("mask stub %s missing: %v", d, err)
		} else if !fi.IsDir() {
			t.Errorf("mask stub %s should be a dir, got a file", d)
		}
	}
	// Create-only: a re-run (as after resetDirContents on the next start) must not
	// error, and must not truncate a stub that gained content.
	if err := os.WriteFile(filepath.Join(cwd, ".bashrc"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := materializeSandboxMaskStubs(cwd); err != nil {
		t.Fatalf("re-run should be idempotent: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, ".bashrc")); err != nil || string(b) != "keep" {
		t.Errorf("mask-stub re-run truncated an existing file: %q err=%v", b, err)
	}
}

func TestBuildSettingsPinsHookBinary(t *testing.T) {
	// When HookBinary is set (the dispatcher pins it to os.Executable()), the hook
	// command runs that exact absolute path, shell-quoted so a space is safe.
	s := buildSettings(scaffoldParams{CacheDir: "/c", HookBinary: "/opt/my bin/ak-tgclaude"})
	cmd := s.Hooks.PreToolUse[0].Hooks[0].Command
	if !strings.HasPrefix(cmd, "'/opt/my bin/ak-tgclaude' hook pretooluse") {
		t.Errorf("hook binary not pinned/quoted: %q", cmd)
	}
}

func TestBuildSettingsBangBug(t *testing.T) {
	// Off by default: no --bang-bug in the hook command.
	off := buildSettings(scaffoldParams{CacheDir: "/c"})
	if strings.Contains(off.Hooks.PreToolUse[0].Hooks[0].Command, "--bang-bug") {
		t.Errorf("default should not pass --bang-bug: %q", off.Hooks.PreToolUse[0].Hooks[0].Command)
	}
	// On: the flag is appended to the generated hook command.
	on := buildSettings(scaffoldParams{CacheDir: "/c", BangBug: true})
	if !strings.Contains(on.Hooks.PreToolUse[0].Hooks[0].Command, "--bang-bug") {
		t.Errorf("BangBug should pass --bang-bug: %q", on.Hooks.PreToolUse[0].Hooks[0].Command)
	}
}

func TestBuildSettingsDeniesTranscriptRoot(t *testing.T) {
	// The whole transcript root is deny-read at the Bash layer (each invocation
	// carves its own scope back via the allowRead overlay).
	s := buildSettings(scaffoldParams{CacheDir: "/c", TranscriptRoot: "/s/transcripts"})
	found := false
	for _, p := range s.Sandbox.Filesystem.DenyRead {
		if p == "/s/transcripts" {
			found = true
		}
	}
	if !found {
		t.Errorf("transcript root should be deny-read: %v", s.Sandbox.Filesystem.DenyRead)
	}
	// Absent when the feature is off.
	off := buildSettings(scaffoldParams{CacheDir: "/c"})
	for _, p := range off.Sandbox.Filesystem.DenyRead {
		if p == "/s/transcripts" {
			t.Errorf("no transcript deny-read when off: %v", off.Sandbox.Filesystem.DenyRead)
		}
	}
}

func TestBuildSettingsDenyRead(t *testing.T) {
	// Operator deny_reads land in the sandbox's Bash-layer denyRead. The Read-TOOL
	// layer gets the same paths (plus the token) via the dispatcher's file-policy env
	// — no longer baked into the hook command (see the responder denyPaths tests).
	s := buildSettings(scaffoldParams{
		CacheDir:   "/c",
		OutboxRoot: "/run/out",
		TokenFile:  "/cfg/bot.toml",
		DenyRead:   []string{"/secret/a", "~/b"},
	})

	// Bash layer: operator paths, then the outbox root (host secrets are now the
	// whole-directory ~/.claude / ~/.ssh deny in credentials.files, not here).
	want := []string{"/secret/a", "~/b", "/run/out"}
	got := s.Sandbox.Filesystem.DenyRead
	if len(got) != len(want) {
		t.Fatalf("denyRead = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("denyRead[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}

	// The hook command no longer bakes deny paths — they ride the file-policy env.
	if cmd := s.Hooks.PreToolUse[0].Hooks[0].Command; strings.Contains(cmd, "--deny-read") {
		t.Errorf("deny paths must not remain in the hook command: %q", cmd)
	}
}

func TestBuildSettingsDenyEnvsAdditive(t *testing.T) {
	// The default secrets are ALWAYS scrubbed; operator DenyEnvVars are additive
	// and de-duplicated (denying an already-default var must not double it).
	s := buildSettings(scaffoldParams{
		CacheDir:    "/c",
		DenyEnvVars: []string{"MY_SECRET", "ANTHROPIC_API_KEY"},
	})
	var names []string
	for _, e := range s.Sandbox.Credentials.EnvVars {
		if e.Mode != "deny" {
			t.Errorf("env var not deny: %+v", e)
		}
		names = append(names, e.Name)
	}
	// defaults first (3), then the new MY_SECRET; the duplicate ANTHROPIC_API_KEY
	// is dropped.
	want := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "MY_SECRET"}
	if len(names) != len(want) {
		t.Fatalf("deny env names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("deny env[%d] = %q, want %q (full %v)", i, names[i], want[i], names)
		}
	}
}

func TestBuildSettingsAllowDomainsAdditive(t *testing.T) {
	// The Go-build defaults are ALWAYS present; operator NetworkDomains (allow_domains)
	// are additive and de-duplicated (an already-default domain must not double), with
	// the defaults first.
	s := buildSettings(scaffoldParams{
		CacheDir:       "/c",
		NetworkDomains: []string{"api.github.com", "proxy.golang.org"},
	})
	got := s.Sandbox.Network.AllowedDomains
	// defaults first (3), then the new api.github.com; the duplicate proxy.golang.org
	// is dropped.
	want := []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "api.github.com"}
	if len(got) != len(want) {
		t.Fatalf("allowed domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("allowed domain[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildSettingsAllowDomainsDefaultsWhenEmpty(t *testing.T) {
	// No operator domains => exactly the Go-build defaults (the additive merge must
	// not drop the empty case).
	s := buildSettings(scaffoldParams{CacheDir: "/c"})
	got := s.Sandbox.Network.AllowedDomains
	want := []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
	if len(got) != len(want) {
		t.Fatalf("allowed domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("allowed domain[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUploadNote(t *testing.T) {
	// Off (no command) => empty, so the {{UPLOAD_NOTE}} marker vanishes.
	if got := uploadNote("", 40, 300); got != "" {
		t.Errorf("no command => empty note, got %q", got)
	}
	// On with an advertised max => both numbers appear and the ceiling is stated.
	on := uploadNote("cmd", 40, 300)
	if !strings.Contains(on, "~40 MB") || !strings.Contains(on, "~300 MB") {
		t.Errorf("note missing threshold/max: %q", on)
	}
	if !strings.HasSuffix(on, "\n\n") {
		t.Errorf("note must carry its own trailing blank line: %q", on)
	}
	// On without a max => the threshold line, but no advertised ceiling.
	noMax := uploadNote("cmd", 40, 0)
	if !strings.Contains(noMax, "~40 MB") || strings.Contains(noMax, "up to") {
		t.Errorf("no-max note wrong: %q", noMax)
	}
}

func TestInjectUploadNote(t *testing.T) {
	in := []byte("done.\n\n{{UPLOAD_NOTE}}## Next")
	if got := string(injectUploadNote(in, "")); got != "done.\n\n## Next" {
		t.Errorf("off: %q", got)
	}
	if got := string(injectUploadNote(in, "NOTE\n\n")); got != "done.\n\nNOTE\n\n## Next" {
		t.Errorf("on: %q", got)
	}
}

func TestMaterializeScaffoldUploadNote(t *testing.T) {
	skillPath := func(cwd string) string {
		return filepath.Join(cwd, ".claude", "skills", "tg-emit", "SKILL.md")
	}
	// On: the note lands in tg-emit and the marker is gone.
	on := t.TempDir()
	if err := materializeScaffold(on, scaffoldParams{CacheDir: "/c", UploadNote: uploadNote("cmd", 40, 300)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(skillPath(on))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "{{UPLOAD_NOTE}}") || !strings.Contains(s, "~300 MB") {
		t.Errorf("note not injected / marker left: has-marker=%t has-note=%t", strings.Contains(s, "{{UPLOAD_NOTE}}"), strings.Contains(s, "~300 MB"))
	}
	// Off: marker vanishes, no note text.
	off := t.TempDir()
	if err := materializeScaffold(off, scaffoldParams{CacheDir: "/c"}); err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(skillPath(off))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b2); strings.Contains(s, "{{UPLOAD_NOTE}}") || strings.Contains(s, "Large files") {
		t.Errorf("off: marker or note should be absent")
	}
}

func TestBuildSettingsNoTokenFile(t *testing.T) {
	s := buildSettings(scaffoldParams{CacheDir: "/c"})
	// Even without a bot token, the host secrets are always denied; only the bot
	// config file is absent.
	files := s.Sandbox.Credentials.Files
	if len(files) != 2 || files[0].Path != "~/.ssh" || files[1].Path != "~/.claude" {
		t.Errorf("host secrets should always be denied (no token), got %+v", files)
	}
}

func TestScaffoldParamsBotTokenEnv(t *testing.T) {
	// Token via env var: scrub the var NAME from the responder, and deny NO token
	// file (there is none — a bare-file token deny is exactly what we avoid).
	c := &Config{BotTokenEnv: "MY_BOT_TOKEN", ConfigPath: "/etc/bot.toml", DenyEnvs: []string{"OTHER"}}
	p := c.scaffoldParams("/c", "/run/out")
	if p.TokenFile != "" {
		t.Errorf("env-token config must not deny a token file, TokenFile=%q", p.TokenFile)
	}
	if !contains(p.DenyEnvVars, "MY_BOT_TOKEN") {
		t.Errorf("bot_token_env name must be scrubbed, DenyEnvVars=%v", p.DenyEnvVars)
	}
	s := buildSettings(scaffoldParams{CacheDir: "/c", TokenFile: p.TokenFile, DenyEnvVars: p.DenyEnvVars})
	found := false
	for _, e := range s.Sandbox.Credentials.EnvVars {
		if e.Name == "MY_BOT_TOKEN" && e.Mode == "deny" {
			found = true
		}
	}
	if !found {
		t.Errorf("MY_BOT_TOKEN not scrubbed in credentials.envVars: %+v", s.Sandbox.Credentials.EnvVars)
	}
	for _, f := range s.Sandbox.Credentials.Files {
		if f.Path == "/etc/bot.toml" {
			t.Error("env-token config must not deny the config file as a token file")
		}
	}
	// Contrast: an inline token still denies the config file that holds it.
	if p2 := (&Config{BotToken: "raw", ConfigPath: "/etc/bot.toml"}).scaffoldParams("/c", "/o"); p2.TokenFile != "/etc/bot.toml" {
		t.Errorf("inline token should still deny the config file, TokenFile=%q", p2.TokenFile)
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

// composedPersona returns the persona text loadPolicies composes for the given
// selectors — exactly what the dispatcher injects via --append-system-prompt.
func composedPersona(t *testing.T, policies ...string) string {
	t.Helper()
	b, err := loadPolicies(policies)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// neutralAgentBody materializes the scaffold and returns the (now persona-neutral)
// responder agent body.
func neutralAgentBody(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c"}); err != nil {
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
	if err := materializeFile(p, []byte("root={{PROJECT}}/x"), "/proj", 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "root=/proj/x" {
		t.Errorf("substitution: got %q, want %q", b, "root=/proj/x")
	}
	// Empty project leaves the placeholder visible (not a broken empty path).
	q := filepath.Join(dir, "b.md")
	if err := materializeFile(q, []byte("root={{PROJECT}}/x"), "", 0o600); err != nil {
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

func TestTranscriptSkillPreloadedWhenOn(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", TranscriptRoot: "/s/transcripts"}); err != nil {
		t.Fatal(err)
	}
	// The skill body is materialized...
	if _, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "tg-recall", "SKILL.md")); err != nil {
		t.Errorf("tg-recall skill not materialized: %v", err)
	}
	// ...and preloaded into the agent alongside the built-in tg-emit.
	agent, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "skills: [tg-emit, tg-recall]") {
		t.Errorf("agent should preload tg-recall: %q", string(agent))
	}
}

func TestTranscriptSkillAbsentWhenOff(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "tg-recall")); !os.IsNotExist(err) {
		t.Errorf("tg-recall should not be materialized when the feature is off (err=%v)", err)
	}
	agent, _ := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if strings.Contains(string(agent), "tg-recall") {
		t.Errorf("agent should not mention tg-recall when off: %q", string(agent))
	}
}

func TestUsageSkillMaterializedButNotPreloadedWhenOn(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", UsageLogOn: true}); err != nil {
		t.Fatal(err)
	}
	// The skill body is materialized (available on demand)...
	if _, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "tg-usage", "SKILL.md")); err != nil {
		t.Errorf("tg-usage skill not materialized when on: %v", err)
	}
	// ...but deliberately NOT preloaded into the agent frontmatter (owner-only, rare —
	// Anton's explicit ask). The agent must list only the built-in tg-emit.
	agent, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(agent), "tg-usage") {
		t.Errorf("agent must NOT preload tg-usage (available-only): %q", string(agent))
	}
}

func TestUsageSkillAbsentWhenOff(t *testing.T) {
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "tg-usage")); !os.IsNotExist(err) {
		t.Errorf("tg-usage should not be materialized when the feature is off (err=%v)", err)
	}
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

func TestWireSkillRequiresDir(t *testing.T) {
	src := t.TempDir()
	skillDir := writeSkill(t, src, "my-skill", "---\nname: my-skill\n---\nbody")

	// A directory wires fine (its basename is the skill name).
	claudeDir := filepath.Join(t.TempDir(), ".claude")
	names, err := wireSkills(claudeDir, "/proj", []string{skillDir})
	if err != nil {
		t.Fatalf("wireSkills(dir): %v", err)
	}
	if len(names) != 1 || names[0] != "my-skill" {
		t.Errorf("names = %v, want [my-skill]", names)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "my-skill", "SKILL.md")); err != nil {
		t.Errorf("skill not materialized: %v", err)
	}

	// A bare SKILL.md file is rejected — copying only it would drop the skill's
	// siblings. The error points the operator at the parent directory.
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if _, err := wireSkills(filepath.Join(t.TempDir(), ".claude"), "/proj", []string{skillFile}); err == nil {
		t.Errorf("wireSkills(file) should reject a bare SKILL.md, got nil error")
	} else if !strings.Contains(err.Error(), "must be a skill DIRECTORY") {
		t.Errorf("unexpected error for file input: %v", err)
	}
}

func TestWireSkillPreservesExecBit(t *testing.T) {
	// A skill with a bundled executable (e.g. selftest.sh) must arrive runnable —
	// materializeFile used to hardcode 0o600, silently stripping +x.
	src := writeSkill(t, t.TempDir(), "toolful",
		"---\nname: toolful\n---\nRun ${CLAUDE_SKILL_DIR}/selftest.sh\n")
	script := filepath.Join(src, "selftest.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(src, "notes.md")
	if err := os.WriteFile(plain, []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(t.TempDir(), ".claude")
	if _, err := wireSkills(claudeDir, "/proj", []string{src}); err != nil {
		t.Fatalf("wireSkills: %v", err)
	}

	dstScript := filepath.Join(claudeDir, "skills", "toolful", "selftest.sh")
	fi, err := os.Stat(dstScript)
	if err != nil {
		t.Fatalf("bundled script not materialized: %v", err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("executable bit lost: mode = %v, want owner-exec set", fi.Mode())
	}
	// A plain sibling stays non-executable (owner-only 0o600).
	dfi, err := os.Stat(filepath.Join(claudeDir, "skills", "toolful", "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if dfi.Mode()&0o111 != 0 {
		t.Errorf("plain file should not be executable: mode = %v", dfi.Mode())
	}
}

func TestAddSkillVerbatimNoPreload(t *testing.T) {
	// A generic skill is copied verbatim: {{PROJECT}} is NOT substituted (it is
	// project-agnostic), an executable bundled file keeps +x, and — crucially — the
	// skill is NOT preloaded into the agent's skills: (it is on-demand).
	src := writeSkill(t, t.TempDir(), "generic-tool",
		"---\nname: generic-tool\n---\nRefs {{PROJECT}}/x and ${CLAUDE_SKILL_DIR}/run.sh\n")
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{
		CacheDir:  "/c",
		Project:   "/home/me/thoughts",
		AddSkills: []string{src},
	}); err != nil {
		t.Fatalf("materializeScaffold: %v", err)
	}

	// Copied verbatim — {{PROJECT}} stays literal (no substitution for generics).
	got, err := os.ReadFile(filepath.Join(cwd, ".claude", "skills", "generic-tool", "SKILL.md"))
	if err != nil {
		t.Fatalf("added skill not materialized: %v", err)
	}
	if !strings.Contains(string(got), "{{PROJECT}}/x") {
		t.Errorf("add-skill must NOT substitute {{PROJECT}}:\n%s", got)
	}
	// Bundled executable keeps +x.
	fi, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "generic-tool", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("added skill lost +x: %v", fi.Mode())
	}
	// NOT preloaded: the agent's skills: must not list it (only built-in tg-emit).
	agent, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(agent), "generic-tool") {
		t.Errorf("add-skill must NOT be preloaded into the agent:\n%s", agent)
	}
}

func TestAddSkillRequiresDir(t *testing.T) {
	skillDir := writeSkill(t, t.TempDir(), "g", "---\nname: g\n---\nbody")
	err := addSkills(filepath.Join(t.TempDir(), ".claude"), []string{filepath.Join(skillDir, "SKILL.md")})
	if err == nil {
		t.Fatal("addSkills should reject a bare SKILL.md file")
	}
	if !strings.Contains(err.Error(), "must be a skill DIRECTORY") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAddAgentCopiesFileRejectsDir(t *testing.T) {
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "helper.md")
	if err := os.WriteFile(agentFile, []byte("---\nname: helper\n---\nagent body {{PROJECT}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(t.TempDir(), ".claude")
	if err := addAgents(claudeDir, []string{agentFile}); err != nil {
		t.Fatalf("addAgents(file): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(claudeDir, "agents", "helper.md"))
	if err != nil {
		t.Fatalf("agent not copied: %v", err)
	}
	// Verbatim: {{PROJECT}} left untouched (generic agent).
	if !strings.Contains(string(got), "{{PROJECT}}") {
		t.Errorf("add-agent must copy verbatim:\n%s", got)
	}
	// A directory is rejected (agents are single .md files).
	if err := addAgents(filepath.Join(t.TempDir(), ".claude"), []string{dir}); err == nil {
		t.Error("addAgents should reject a directory")
	} else if !strings.Contains(err.Error(), "must be an agent .md FILE") {
		t.Errorf("unexpected error: %v", err)
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

func TestMaterializeAgentIsPersonaNeutral(t *testing.T) {
	// The materialized agent keeps its name and shared mechanics but carries NO
	// persona: the {{POLICY}} marker is gone and no stance text is baked in (the
	// persona now rides --append-system-prompt at spawn).
	body := neutralAgentBody(t)
	if !strings.Contains(body, "name: "+defaultAgent) {
		t.Errorf("agent lost its name:\n%s", body)
	}
	if strings.Contains(body, policyPlaceholder) {
		t.Errorf("{{POLICY}} marker survived in the neutral agent:\n%s", body)
	}
	if !strings.Contains(body, "## Replying") || !strings.Contains(body, "## Boundaries") {
		t.Errorf("agent dropped the shared base sections:\n%s", body)
	}
	// No persona stance is baked in (these strings are persona-only).
	for _, persona := range []string{"out of scope", "do-what-you're-asked", "introspection / debug"} {
		if strings.Contains(body, persona) {
			t.Errorf("persona text %q leaked into the neutral agent:\n%s", persona, body)
		}
	}
}

func TestComposePolicyText(t *testing.T) {
	// "" defaults to normal; each selector composes to its distinctive persona text.
	def := composedPersona(t, "")
	nr := composedPersona(t, "norefuse")
	intro := composedPersona(t, "introspect")
	// Frontmatter (axis:) is stripped from the composed text.
	for _, p := range []string{def, nr, intro} {
		if strings.Contains(p, "axis:") || strings.HasPrefix(p, "---") {
			t.Errorf("frontmatter leaked into composed persona:\n%s", p)
		}
	}
	// normal (the default) declines off-topic and carries the untrusted-input framing.
	if !strings.Contains(def, "out of scope") || !strings.Contains(def, "untrusted") {
		t.Errorf("normal policy should scope + treat input as untrusted:\n%s", def)
	}
	// norefuse says not to decline and drops the untrusted framing.
	if !strings.Contains(nr, "NOT** decline") {
		t.Errorf("norefuse policy should say not to decline:\n%s", nr)
	}
	if strings.Contains(nr, "untrusted") {
		t.Errorf("norefuse policy should not carry the untrusted-input framing:\n%s", nr)
	}
	// introspect is the candid/debug persona.
	if !strings.Contains(intro, "introspect") || !strings.Contains(intro, "precise") {
		t.Errorf("introspect policy should be the candid/debug persona:\n%s", intro)
	}
}

func TestComposePolicyCustomFile(t *testing.T) {
	// A --policy path composes an operator's own fragment.
	f := filepath.Join(t.TempDir(), "my-policy.md")
	if err := os.WriteFile(f, []byte("You are a CUSTOM persona for this bot.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := composedPersona(t, f); !strings.Contains(body, "You are a CUSTOM persona for this bot.") {
		t.Errorf("custom policy fragment not composed in:\n%s", body)
	}
	// An unknown built-in NAME (not a path) is an error, not a silent miss.
	if _, err := loadPolicies([]string{"bogus"}); err == nil {
		t.Errorf("unknown policy name should error")
	}
}

func TestMaterializeAgentMergesPolicies(t *testing.T) {
	// Several selectors merge in order into ONE persona: both fragments' distinctive
	// prose is present, the marker is gone, and a custom .md layers on top of a
	// built-in.
	f := filepath.Join(t.TempDir(), "extra.md")
	if err := os.WriteFile(f, []byte("EXTRA persona layered on top.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := composedPersona(t, "norefuse", f)
	if !strings.Contains(body, "NOT** decline") {
		t.Errorf("merged policy dropped the norefuse fragment:\n%s", body)
	}
	if !strings.Contains(body, "EXTRA persona layered on top.") {
		t.Errorf("merged policy dropped the custom fragment:\n%s", body)
	}
	// A blank line separates the two fragments (norefuse body then the custom one).
	if !strings.Contains(body, "\n\nEXTRA persona layered on top.") {
		t.Errorf("merged fragments not blank-line separated:\n%s", body)
	}
}

func TestParseFragment(t *testing.T) {
	// Frontmatter is stripped; axis and summary are read into the fields map.
	fields, body := parseFragment([]byte("---\naxis: refusal\nsummary: a scoped FAQ\n---\nYou are strict.\n"))
	if fields["axis"] != "refusal" {
		t.Errorf("axis = %q, want refusal", fields["axis"])
	}
	if fields["summary"] != "a scoped FAQ" {
		t.Errorf("summary = %q, want %q", fields["summary"], "a scoped FAQ")
	}
	if strings.TrimSpace(string(body)) != "You are strict." {
		t.Errorf("body = %q, want the persona text without frontmatter", body)
	}
	// No frontmatter => empty fields, the whole thing is body.
	if f, b := parseFragment([]byte("Just a persona.\n")); f["axis"] != "" || strings.TrimSpace(string(b)) != "Just a persona." {
		t.Errorf("plain fragment: axis=%q body=%q", f["axis"], b)
	}
	// A leading fence with no closing fence is all body (no panic, no fields).
	if f, _ := parseFragment([]byte("---\nnot really frontmatter\n")); f["axis"] != "" {
		t.Errorf("unterminated frontmatter should yield no axis, got %q", f["axis"])
	}
	// A quoted axis value is unquoted.
	if f, _ := parseFragment([]byte("---\naxis: \"refusal\"\n---\nx")); f["axis"] != "refusal" {
		t.Errorf("quoted axis = %q, want refusal", f["axis"])
	}
}

func TestPolicySummary(t *testing.T) {
	// Every built-in ships a non-empty summary (backs `--policy help`).
	for _, p := range builtinPolicyOrder {
		s, err := policySummary(p)
		if err != nil {
			t.Fatalf("policySummary(%q): %v", p, err)
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("built-in %q has no summary:", p)
		}
	}
	// A fragment without a summary field yields "".
	f := filepath.Join(t.TempDir(), "no-summary.md")
	if err := os.WriteFile(f, []byte("You are a custom persona.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, _ := policySummary(f); s != "" {
		t.Errorf("summary of a fragment without one = %q, want empty", s)
	}
}

func TestPrintPolicyCatalog(t *testing.T) {
	var sb strings.Builder
	if err := printPolicyCatalog(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	// Every built-in name and its summary appear, in order, plus the custom-fragment note.
	last := -1
	for _, p := range builtinPolicyOrder {
		i := strings.Index(out, p)
		if i < 0 {
			t.Errorf("catalog missing policy %q:\n%s", p, out)
		}
		if i < last {
			t.Errorf("catalog lists %q out of builtinPolicyOrder:\n%s", p, out)
		}
		last = i
		s, _ := policySummary(p)
		if !strings.Contains(out, s) {
			t.Errorf("catalog missing summary for %q:\n%s", p, out)
		}
	}
	if !strings.Contains(out, "path to your own .md fragment") {
		t.Errorf("catalog should mention custom fragments:\n%s", out)
	}
}

func TestOutboxRWPolicy(t *testing.T) {
	// outbox-rw is a recognized built-in, axis-less (additive), and composes its
	// distinctive outbox/clone guidance.
	if !builtinPolicies["outbox-rw"] {
		t.Fatal("outbox-rw should be a built-in policy")
	}
	if axis, err := policyAxis("outbox-rw"); err != nil || axis != "" {
		t.Errorf("outbox-rw axis = %q err=%v, want axis-less", axis, err)
	}
	// Axis-less => it stacks on any refusal stance without a conflict.
	if err := checkAxisConflicts([]string{"strict", "outbox-rw"}); err != nil {
		t.Errorf("strict + outbox-rw should not conflict: %v", err)
	}
	body := composedPersona(t, "strict", "outbox-rw")
	if !strings.Contains(body, "outbox") || !strings.Contains(body, "git clone --shared") {
		t.Errorf("outbox-rw persona missing its outbox/clone guidance:\n%s", body)
	}
}

func TestWithDefaultStance(t *testing.T) {
	// An axis-less-only list gets normal prepended as the base stance.
	for _, name := range []string{"introspect", "outbox-rw"} {
		got, err := withDefaultStance([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "normal" || got[1] != name {
			t.Errorf("withDefaultStance([%s]) = %v, want [normal %s]", name, got, name)
		}
	}
	// A list that already carries a refusal-axis fragment is left untouched.
	for _, in := range [][]string{{"strict"}, {"norefuse"}, {"normal"}, {"strict", "outbox-rw"}} {
		got, err := withDefaultStance(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(in) || got[0] != in[0] {
			t.Errorf("withDefaultStance(%v) = %v, want unchanged", in, got)
		}
	}
	// An empty list is returned unchanged (loadPolicies maps it to defaultPolicy).
	if got, _ := withDefaultStance(nil); len(got) != 0 {
		t.Errorf("withDefaultStance(nil) = %v, want empty", got)
	}
	// A custom fragment declaring axis: refusal occupies the slot — no floor.
	fr := filepath.Join(t.TempDir(), "myrefusal.md")
	if err := os.WriteFile(fr, []byte("---\naxis: refusal\n---\nMy base.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := withDefaultStance([]string{fr})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != fr {
		t.Errorf("axis:refusal custom = %v, want [%q] (no normal floor)", got, fr)
	}
}

func TestCheckAxisConflicts(t *testing.T) {
	// Two refusal-axis built-ins conflict.
	if err := checkAxisConflicts([]string{"normal", "norefuse"}); err == nil {
		t.Errorf("normal + norefuse should conflict on axis refusal")
	}
	// One refusal + an axis-less one is fine.
	if err := checkAxisConflicts([]string{"strict", "introspect"}); err != nil {
		t.Errorf("strict + introspect should not conflict: %v", err)
	}
	// A single fragment never conflicts.
	if err := checkAxisConflicts([]string{"norefuse"}); err != nil {
		t.Errorf("single fragment should not conflict: %v", err)
	}
}

func TestResolveEffectivePolicies(t *testing.T) {
	// An override on the shared axis EVICTS the default fragment in place; an
	// axis-less default (introspect) is untouched.
	got, err := resolveEffectivePolicies([]string{"strict", "introspect"}, []string{"norefuse"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "norefuse" || got[1] != "introspect" {
		t.Errorf("eviction in place = %v, want [norefuse introspect]", got)
	}
	// An axis-less override just appends (nothing to evict).
	if got, _ := resolveEffectivePolicies([]string{"strict"}, []string{"introspect"}); len(got) != 2 || got[0] != "strict" || got[1] != "introspect" {
		t.Errorf("axis-less append = %v, want [strict introspect]", got)
	}
	// An empty override yields the base unchanged.
	if got, _ := resolveEffectivePolicies([]string{"strict"}, nil); len(got) != 1 || got[0] != "strict" {
		t.Errorf("empty override = %v, want [strict]", got)
	}
}

func TestInjectMCPTools(t *testing.T) {
	const tmpl = "---\nname: x\ntools: Read, Skill{{MCP_TOOLS}}\n---\nbody"

	// Non-empty: appended with a leading separator; no marker left behind.
	got := string(injectMCPTools([]byte(tmpl), []string{"mcp__tg__a", "mcp__tg__b"}))
	if want := "tools: Read, Skill, mcp__tg__a, mcp__tg__b\n"; !strings.Contains(got, want) {
		t.Errorf("non-empty inject: got %q, want a line containing %q", got, want)
	}
	if strings.Contains(got, mcpToolsPlaceholder) {
		t.Errorf("placeholder left unsubstituted:\n%s", got)
	}

	// Empty: the marker vanishes with NO dangling comma (the concern that motivated
	// putting the separator inside the expansion).
	empty := string(injectMCPTools([]byte(tmpl), nil))
	if want := "tools: Read, Skill\n"; !strings.Contains(empty, want) {
		t.Errorf("empty inject should leave a clean list: got %q, want a line containing %q", empty, want)
	}
	if strings.Contains(empty, "Skill,") {
		t.Errorf("empty inject left a dangling comma:\n%s", empty)
	}
}

func TestMaterializeAgentInjectsMCPTools(t *testing.T) {
	// The tools: line (in the shared, persona-neutral base) gets the real send tools
	// substituted from the single mcpTools source, no {{MCP_TOOLS}} surviving.
	body := neutralAgentBody(t)
	if strings.Contains(body, mcpToolsPlaceholder) {
		t.Errorf("{{MCP_TOOLS}} left unsubstituted:\n%s", body)
	}
	want := "tools: Read, Bash, Write, Edit, Skill, " + strings.Join(mcpTools, ", ")
	if !strings.Contains(body, want) {
		t.Errorf("MCP tools not appended to tools: as expected\nwant line: %q\ngot:\n%s", want, body)
	}
}

func TestMaterializeAgentGrantsExtraTools(t *testing.T) {
	// Operator extra tools (config `tools`/--tool) land in the tools: frontmatter
	// after the send tools, deduped — so availability matches the --allowedTools
	// grant. A duplicate of a send tool is not repeated.
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c", Tools: []string{"Agent", "mcp__tg__send_message"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "tools: Read, Bash, Write, Edit, Skill, " + strings.Join(mcpTools, ", ") + ", Agent"
	if body := string(b); !strings.Contains(body, want) {
		t.Errorf("extra tool not in tools: frontmatter (or dedup failed)\nwant line: %q\ngot:\n%s", want, body)
	}
}

func TestBaseToolName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"WebFetch(domain:*.github.com)", "WebFetch"}, // scope stripped
		{"Bash(git *)", "Bash"},
		{"Agent", "Agent"}, // bare name unchanged
		{"mcp__tg__send_message", "mcp__tg__send_message"},
		{"mcp__x__*", "mcp__x__*"}, // a "*" with no parens is a pattern, not a scope — kept
		{"  WebFetch(domain:x)  ", "WebFetch"},
	} {
		if got := baseToolName(c.in); got != c.want {
			t.Errorf("baseToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFrontmatterToolsDedupsByVerb(t *testing.T) {
	// Two scopes of the same verb collapse to a single bare name (order-preserving);
	// a bare name and a scoped spec of the same verb also collapse; non-scoped
	// entries pass through unchanged.
	in := []string{"WebFetch(domain:a)", "Agent", "WebFetch(domain:b)", "WebFetch", "mcp__tg__x"}
	got := strings.Join(frontmatterTools(in), ",")
	want := "WebFetch,Agent,mcp__tg__x"
	if got != want {
		t.Errorf("frontmatterTools(%v) = %q, want %q", in, got, want)
	}
}

func TestMaterializeAgentScopedExtraToolBareInFrontmatter(t *testing.T) {
	// A scoped extra tool lands in the tools: frontmatter as its BARE name (the
	// availability list is keyed by name; the scope rides --allowedTools, not the
	// YAML). Two scopes of the same verb must NOT duplicate the verb (Anton's rule).
	cwd := t.TempDir()
	if err := materializeScaffold(cwd, scaffoldParams{CacheDir: "/c",
		Tools: []string{"WebFetch(domain:github.com)", "WebFetch(domain:*.github.com)", "Agent"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".claude", "agents", defaultAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	// A single bare WebFetch (the two scopes deduped), then Agent — no scoped spec.
	want := "tools: Read, Bash, Write, Edit, Skill, " + strings.Join(mcpTools, ", ") + ", WebFetch, Agent"
	if !strings.Contains(body, want) {
		t.Errorf("scoped tool not reduced to a single bare name in frontmatter\nwant line: %q\ngot:\n%s", want, body)
	}
	// The scope (parens, "*") must never leak into the YAML tools: line.
	if strings.Contains(body, "WebFetch(") {
		t.Errorf("scoped spec leaked into frontmatter tools: line:\n%s", body)
	}
}
