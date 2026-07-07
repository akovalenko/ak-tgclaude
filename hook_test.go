package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setFilePolicyEnv marshals p to JSON and sets it as the hook's policy env, as the
// dispatcher does when spawning the responder.
func setFilePolicyEnv(t *testing.T, p hookFilePolicy) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(filePolicyEnv, string(b))
}

func fileInput(tool, path string) *preToolUseInput {
	in := &preToolUseInput{ToolName: tool}
	in.ToolInput.FilePath = path
	return in
}

func bashInput(cmd string, disableSandbox bool) *preToolUseInput {
	in := &preToolUseInput{ToolName: "Bash"}
	in.ToolInput.Command = cmd
	in.ToolInput.DangerouslyDisableSandbox = disableSandbox
	return in
}

var testPolicy = filePolicy{
	deny:       []string{"/host/.ssh", "/cfg/bot.toml"},    // host secret + token: never
	readAllow:  []string{"/run/out/outbox-A1", "/s/tr/42"}, // own outbox + own transcript
	readDeny:   []string{"/run/out", "/s/tr"},              // sibling outboxes + transcript roots
	writeRoots: []string{"/run/out/outbox-A1"},             // writes: the outbox only
}

func TestDecideReadMirrorsSandbox(t *testing.T) {
	// Read is default-OPEN — the project and any ordinary external file are allowed,
	// exactly as sandboxed Bash reads them: no project confinement, no cat-via-Bash.
	for _, p := range []string{"/proj/main.go", "/etc/hostname", "/var/log/x", "/home/other/notes.txt"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), testPolicy); d != "allow" {
			t.Errorf("read %q => %q, want allow (default-open)", p, d)
		}
	}
	// This invocation's own outbox and transcript: readable via the carve.
	for _, p := range []string{"/run/out/outbox-A1/reply.md", "/s/tr/42/2026.jsonl"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), testPolicy); d != "allow" {
			t.Errorf("read own scope %q => %q, want allow", p, d)
		}
	}
	// A sibling's outbox / another chat's transcript: masked (under a readDeny root,
	// not carved) — the cross-chat isolation the project allowlist used to give.
	for _, p := range []string{"/run/out/outbox-B2/steal.md", "/s/tr/99/2026.jsonl"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), testPolicy); d != "deny" {
			t.Errorf("read sibling %q => %q, want deny (masked)", p, d)
		}
	}
	// Host secret + token: absolute deny, wins over the default-open.
	for _, p := range []string{"/host/.ssh/id_rsa", "/cfg/bot.toml"} {
		if d, _ := decidePreToolUse(fileInput("Read", p), testPolicy); d != "deny" {
			t.Errorf("read secret %q => %q, want deny", p, d)
		}
	}
}

// TestDecideSymlinkEscapeDenied covers the hook's symlink defense with REAL on-disk
// links (resolution touches the filesystem). A link a responder plants in its own
// outbox has an own-scope LEXICAL path, but its resolved target escapes — so a read
// of a host secret / sibling outbox and a write to a host file are all denied, while
// an ordinary regular file in the own outbox stays allowed.
func TestDecideSymlinkEscapeDenied(t *testing.T) {
	base := t.TempDir()
	outbox := filepath.Join(base, "outbox-A1")
	secretDir := filepath.Join(base, "secret")
	sibling := filepath.Join(base, "outbox-B2")
	for _, d := range []string{outbox, secretDir, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustLink := func(target, link string) {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(secretDir, "id_rsa"), "KEY")
	mustWrite(filepath.Join(sibling, "steal.md"), "theirs")

	// readDeny masks the whole parent (siblings); readAllow carves the own outbox back;
	// deny is the absolute host-secret set — mirrors the real dispatcher-computed policy.
	pol := filePolicy{
		deny:       []string{secretDir},
		readAllow:  []string{outbox},
		readDeny:   []string{base},
		writeRoots: []string{outbox},
	}

	// An ordinary regular file in the own outbox: read + write allowed (the carve).
	reg := filepath.Join(outbox, "reply.md")
	mustWrite(reg, "mine")
	if d, _ := decidePreToolUse(fileInput("Read", reg), pol); d != "allow" {
		t.Errorf("read own regular file => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(fileInput("Write", reg), pol); d != "allow" {
		t.Errorf("write own regular file => %q, want allow", d)
	}
	// A fresh (not-yet-created) write target in the own outbox is still allowed.
	if d, _ := decidePreToolUse(fileInput("Write", filepath.Join(outbox, "new.md")), pol); d != "allow" {
		t.Errorf("write fresh own file => %q, want allow", d)
	}

	// Own-scope symlink → host secret: absolute deny wins.
	mustLink(filepath.Join(secretDir, "id_rsa"), filepath.Join(outbox, "toSecret"))
	if d, _ := decidePreToolUse(fileInput("Read", filepath.Join(outbox, "toSecret")), pol); d != "deny" {
		t.Errorf("read own-scope symlink to a host secret => %q, want deny", d)
	}
	// Own-scope symlink → sibling outbox: masked (resolved target under readDeny).
	mustLink(filepath.Join(sibling, "steal.md"), filepath.Join(outbox, "toSibling"))
	if d, _ := decidePreToolUse(fileInput("Read", filepath.Join(outbox, "toSibling")), pol); d != "deny" {
		t.Errorf("read own-scope symlink to a sibling outbox => %q, want deny", d)
	}
	// Own-scope symlink → host file: the write must not escape the outbox.
	mustWrite(filepath.Join(base, "bashrc"), "orig")
	mustLink(filepath.Join(base, "bashrc"), filepath.Join(outbox, "toBashrc"))
	if d, _ := decidePreToolUse(fileInput("Write", filepath.Join(outbox, "toBashrc")), pol); d != "deny" {
		t.Errorf("write through an own-scope symlink to a host file => %q, want deny", d)
	}
}

// TestDecideDenyRootSymlink covers Scenario A: the deny ROOT is itself a symlink (or
// has a symlinked prefix), the complement of TestDecideSymlinkEscapeDenied's Scenario
// B (a symlinked TOOL path into a real deny dir). Both spellings of the protected
// target must be denied — the lexical one via the link, and the symlink-resolved real
// target via resolveRoots — so a resolved deny root cannot be dodged by naming its
// real location directly.
func TestDecideDenyRootSymlink(t *testing.T) {
	base := t.TempDir()
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustLink := func(target, link string) {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	// Case 1 — the deny root IS a symlink to the real secret dir.
	realSecret := filepath.Join(base, "realsecret")
	mustMkdir(realSecret)
	mustWrite(filepath.Join(realSecret, "id_rsa"), "KEY")
	denyLink := filepath.Join(base, "denyLink")
	mustLink(realSecret, denyLink)

	// Case 2 — a symlinked PREFIX of the deny root (the /run -> /var/run shape).
	realVarRun := filepath.Join(base, "varrun")
	realRunSecret := filepath.Join(realVarRun, "secret")
	mustMkdir(realRunSecret)
	mustWrite(filepath.Join(realRunSecret, "token"), "T")
	runLink := filepath.Join(base, "run") // run -> varrun
	mustLink(realVarRun, runLink)

	pol := filePolicy{
		deny: []string{
			denyLink,                         // a leaf-symlink deny root (Scenario A)
			filepath.Join(runLink, "secret"), // deny root with a symlinked prefix
		},
		readAllow:  []string{},
		readDeny:   []string{},
		writeRoots: []string{},
	}

	// Every spelling of a protected target must be denied.
	for _, p := range []string{
		filepath.Join(denyLink, "id_rsa"),         // via the symlinked deny root (lexical)
		filepath.Join(realSecret, "id_rsa"),       // the resolved real target (via resolveRoots)
		filepath.Join(runLink, "secret", "token"), // via the symlinked prefix (lexical)
		filepath.Join(realRunSecret, "token"),     // the resolved real target
	} {
		if d, _ := decidePreToolUse(fileInput("Read", p), pol); d != "deny" {
			t.Errorf("read protected path %q => %q, want deny", p, d)
		}
	}
}

func TestDecideWriteScopedToOutbox(t *testing.T) {
	if d, _ := decidePreToolUse(fileInput("Write", "/run/out/outbox-A1/reply.html"), testPolicy); d != "allow" {
		t.Errorf("write in outbox => %q, want allow", d)
	}
	// Edit / MultiEdit / NotebookEdit follow the same write policy.
	for _, tool := range []string{"Edit", "MultiEdit", "NotebookEdit"} {
		if d, _ := decidePreToolUse(fileInput(tool, "/run/out/outbox-A1/x"), testPolicy); d != "allow" {
			t.Errorf("%s in outbox => %q, want allow", tool, d)
		}
	}
	// Writing into the project (read-only), /tmp (no longer a scratch root), or
	// anywhere else is denied.
	for _, p := range []string{"/proj/main.go", "/tmp/claude-1000/scratch.md", "/etc/cron.d/x"} {
		if d, _ := decidePreToolUse(fileInput("Write", p), testPolicy); d != "deny" {
			t.Errorf("write %q => %q, want deny", p, d)
		}
	}
}

func TestDecideAbsoluteDenyWinsOverDefaultOpen(t *testing.T) {
	// A protected path is denied even though Read is otherwise default-open (the deny
	// check runs first).
	pol := filePolicy{deny: []string{"/proj/secret.toml"}}
	if d, _ := decidePreToolUse(fileInput("Read", "/proj/secret.toml"), pol); d != "deny" {
		t.Errorf("protected path => %q, want deny", d)
	}
	// A normal file next to it reads fine (default-open).
	if d, _ := decidePreToolUse(fileInput("Read", "/proj/main.go"), pol); d != "allow" {
		t.Errorf("ordinary file => %q, want allow", d)
	}
}

func TestDecideBash(t *testing.T) {
	if d, _ := decidePreToolUse(bashInput("grep foo /proj", false), testPolicy); d != "allow" {
		t.Errorf("sandboxed Bash => %q, want allow", d)
	}
	if d, _ := decidePreToolUse(bashInput("git pull", true), testPolicy); d != "deny" {
		t.Errorf("unsandboxed Bash => %q, want deny", d)
	}
}

func TestDecideBashBangBug(t *testing.T) {
	bangPol := testPolicy
	bangPol.bangBug = true

	// With the guard on, a corrupted `\!` in a sandboxed command is denied.
	if d, r := decidePreToolUse(bashInput(`echo "done\!"`, false), bangPol); d != "deny" || !strings.Contains(r, "#64301") {
		t.Errorf("bang command => %q / %q, want deny citing #64301", d, r)
	}
	// A plain command with no bang is still allowed.
	if d, _ := decidePreToolUse(bashInput("go build ./...", false), bangPol); d != "allow" {
		t.Errorf("no-bang command => %q, want allow", d)
	}
	// A bare `!` (not the corrupted `\!`) is not the signature — only `\!` trips it.
	if d, _ := decidePreToolUse(bashInput("test ! -f x", false), bangPol); d != "allow" {
		t.Errorf("bare bang => %q, want allow (only \\! is the signature)", d)
	}
	// Guard off (default): the same corrupted command is allowed through.
	if d, _ := decidePreToolUse(bashInput(`echo "done\!"`, false), testPolicy); d != "allow" {
		t.Errorf("bang with guard off => %q, want allow", d)
	}
}

func TestDecideDefersOtherTools(t *testing.T) {
	for _, tool := range []string{"Grep", "Glob", "Skill", "WebFetch"} {
		if d, _ := decidePreToolUse(&preToolUseInput{ToolName: tool}, testPolicy); d != "" {
			t.Errorf("%s => %q, want defer (empty)", tool, d)
		}
	}
}

func TestEnvFilePolicy(t *testing.T) {
	setFilePolicyEnv(t, hookFilePolicy{
		WriteRoots: []string{"/run/out/o1"},
		ReadAllow:  []string{"/run/out/o1"},
		ReadDeny:   []string{"/run/out"},
		Deny:       []string{"/cfg/bot.toml"},
	})
	pol := envFilePolicy()

	for _, c := range []struct{ tool, path, want string }{
		{"Read", "/run/out/o1/draft.md", "allow"},  // own outbox: carve
		{"Write", "/run/out/o1/draft.md", "allow"}, // own outbox: writable
		{"Read", "/proj/main.go", "allow"},         // project: default-open
		{"Read", "/etc/hostname", "allow"},         // external: default-open (no dance)
		{"Write", "/proj/main.go", "deny"},         // writes are outbox-only
		{"Read", "/run/out/o2/steal.md", "deny"},   // sibling outbox: masked
		{"Read", "/cfg/bot.toml", "deny"},          // token: absolute deny
	} {
		if d, _ := decidePreToolUse(fileInput(c.tool, c.path), pol); d != c.want {
			t.Errorf("%s %s => %q, want %q", c.tool, c.path, d, c.want)
		}
	}
}

func TestEnvFilePolicyFailSafe(t *testing.T) {
	// Missing or malformed policy => empty roots => every file tool denied (Bash is
	// governed by the sandbox, not this hook, so it is unaffected).
	for _, v := range []string{"", "{not json"} {
		t.Setenv(filePolicyEnv, v)
		pol := envFilePolicy()
		if d, _ := decidePreToolUse(fileInput("Read", "/proj/main.go"), pol); d != "deny" {
			t.Errorf("policy=%q: read => %q, want deny (fail-safe)", v, d)
		}
		if d, _ := decidePreToolUse(fileInput("Write", "/whatever"), pol); d != "deny" {
			t.Errorf("policy=%q: write => %q, want deny (fail-safe)", v, d)
		}
		if d, _ := decidePreToolUse(bashInput("echo hi", false), pol); d != "allow" {
			t.Errorf("policy=%q: sandboxed Bash => %q, want allow (sandbox governs it)", v, d)
		}
	}
}

func TestEnvFilePolicyColonPath(t *testing.T) {
	// JSON round-trips a path holding a ':' exactly — the reason we do not join the
	// list PATH-style (which would split such a path).
	const p = "/run/out/o:1"
	setFilePolicyEnv(t, hookFilePolicy{WriteRoots: []string{p}, ReadAllow: []string{p}})
	pol := envFilePolicy()
	if d, _ := decidePreToolUse(fileInput("Write", p+"/draft.md"), pol); d != "allow" {
		t.Errorf("write under colon-path => %q, want allow (path must round-trip intact)", d)
	}
}

func TestEnvFilePolicyTranscriptScope(t *testing.T) {
	setFilePolicyEnv(t, hookFilePolicy{
		WriteRoots: []string{"/run/out/o1"},
		ReadAllow:  []string{"/run/out/o1", "/s/transcripts/42"},
		ReadDeny:   []string{"/s/transcripts"},
	})
	pol := envFilePolicy()

	// Read is allowed under this chat's own transcript scope (the carve)...
	if d, _ := decidePreToolUse(fileInput("Read", "/s/transcripts/42/2026-07-04.jsonl"), pol); d != "allow" {
		t.Errorf("read own transcript => %q, want allow", d)
	}
	// ...but a sibling chat's dir is under the masked root, not the carve => deny.
	if d, _ := decidePreToolUse(fileInput("Read", "/s/transcripts/99/2026-07-04.jsonl"), pol); d != "deny" {
		t.Errorf("read sibling transcript => %q, want deny", d)
	}
	// The transcript is read-only: a Write is denied (outbox-only writes).
	if d, _ := decidePreToolUse(fileInput("Write", "/s/transcripts/42/x"), pol); d != "deny" {
		t.Errorf("write transcript => %q, want deny", d)
	}
}

func TestUnderAny(t *testing.T) {
	if _, ok := underAny("/a/b/c", []string{"/a/b"}); !ok {
		t.Errorf("file under root should match")
	}
	if _, ok := underAny("/a/b", []string{"/a/b"}); !ok {
		t.Errorf("file equal to root should match")
	}
	if _, ok := underAny("/a/bc", []string{"/a/b"}); ok {
		t.Errorf("prefix-only sibling must not match")
	}
	if _, ok := underAny("", []string{"/a"}); ok {
		t.Errorf("empty file must not match")
	}
	if _, ok := underAny("/x", nil); ok {
		t.Errorf("no roots must not match")
	}
	// Relative file resolves against cwd, then matches an absolute root that
	// contains cwd (best-effort abs/clean).
	abs, _ := filepath.Abs("sub/f")
	if _, ok := underAny(abs, []string{filepath.Dir(filepath.Dir(abs))}); !ok {
		t.Errorf("abs/clean matching failed")
	}
}

func TestAppendHookLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pretooluse.log")
	appendHookLog(p, "first")
	appendHookLog(p, "second")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "first") || !strings.Contains(s, "second") {
		t.Errorf("both entries should be appended: %q", s)
	}
	if n := strings.Count(s, "\n"); n != 2 {
		t.Errorf("expected two log lines, got %d: %q", n, s)
	}
	// A bad path must be swallowed — diagnostic logging never breaks the gate.
	appendHookLog(filepath.Join(t.TempDir(), "no-such-dir", "x.log"), "ignored")
}
