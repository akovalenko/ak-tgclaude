package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory is robust; a bare file is window 2; a missing path is window 1.
func TestAuditSecretsClassifiesByShape(t *testing.T) {
	dir := t.TempDir()
	existingDir := filepath.Join(dir, "secretdir")
	if err := os.Mkdir(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bareFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(bareFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	issues := auditSecrets([]string{existingDir, bareFile, missing}, "")

	byPath := map[string]secretIssueKind{}
	for _, is := range issues {
		byPath[is.Path] = is.Kind
	}
	if k, ok := byPath[existingDir]; ok {
		t.Errorf("directory %s should not be flagged, got kind %v", existingDir, k)
	}
	if k := byPath[bareFile]; k != issueBareFile {
		t.Errorf("bare file: want issueBareFile, got %v", k)
	}
	if k := byPath[missing]; k != issueMissing {
		t.Errorf("missing: want issueMissing, got %v", k)
	}
	if len(issues) != 2 {
		t.Errorf("want 2 issues (bare file + missing), got %d: %+v", len(issues), issues)
	}
}

// A literal-in-file token adds exactly the issueTokenInFile note (and only that when
// the other paths are safe dirs); an empty tokenFile adds nothing.
func TestAuditSecretsTokenInFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "bot.toml")
	if err := os.WriteFile(tokenFile, []byte("bot_token='x'"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeDir := filepath.Join(dir, "safe")
	if err := os.Mkdir(safeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	issues := auditSecrets([]string{safeDir}, tokenFile)
	if len(issues) != 1 {
		t.Fatalf("want 1 issue (token in file), got %d: %+v", len(issues), issues)
	}
	if issues[0].Kind != issueTokenInFile || issues[0].Path != tokenFile {
		t.Errorf("want issueTokenInFile for %s, got %+v", tokenFile, issues[0])
	}

	if got := auditSecrets([]string{safeDir}, ""); len(got) != 0 {
		t.Errorf("empty tokenFile should add no issue, got %+v", got)
	}
}

// Every kind renders a non-empty, self-naming warning (the message the subcommand
// prints and the dispatcher logs).
func TestSecretIssueWarningMentionsPath(t *testing.T) {
	for _, k := range []secretIssueKind{issueMissing, issueBareFile, issueTokenInFile, issueSymlink} {
		w := secretIssue{Path: "/some/secret/path", Kind: k}.warning()
		if !strings.Contains(w, "/some/secret/path") {
			t.Errorf("kind %v: warning should name the path, got %q", k, w)
		}
	}
}

// A protected path that IS a symlink, or lies under one, is flagged issueSymlink —
// checked before the shape classification, so os.Stat (which follows links) cannot
// launder a symlinked secret into a clean-looking directory. The offending link
// component is named. A real directory with no symlinked component stays clean.
func TestAuditSecretsFlagsSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	realFile := filepath.Join(dir, "realfile")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLink := func(target, link string) string {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	linkToDir := mustLink(realDir, filepath.Join(dir, "linkToDir"))    // symlink → directory
	linkToFile := mustLink(realFile, filepath.Join(dir, "linkToFile")) // symlink → file
	dangling := mustLink(filepath.Join(dir, "nope"), filepath.Join(dir, "dangling"))
	// A secret sitting under a symlinked PARENT (the leaf itself is a real dir).
	underParent := filepath.Join(linkToDir, "sub")
	if err := os.Mkdir(filepath.Join(realDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	byPath := map[string]secretIssue{}
	for _, is := range auditSecrets([]string{linkToDir, linkToFile, dangling, underParent, realDir}, "") {
		byPath[is.Path] = is
	}

	// A symlink-to-dir would Stat as a clean directory; it must still be flagged.
	if is, ok := byPath[linkToDir]; !ok || is.Kind != issueSymlink || is.Symlink != linkToDir {
		t.Errorf("symlink-to-dir: want issueSymlink naming %s, got %+v (present=%v)", linkToDir, is, ok)
	}
	// A symlink-to-file is issueSymlink (the more fundamental problem), not issueBareFile.
	if is := byPath[linkToFile]; is.Kind != issueSymlink || is.Symlink != linkToFile {
		t.Errorf("symlink-to-file: want issueSymlink naming %s, got %+v", linkToFile, is)
	}
	// A dangling symlink is issueSymlink, not issueMissing.
	if is := byPath[dangling]; is.Kind != issueSymlink || is.Symlink != dangling {
		t.Errorf("dangling symlink: want issueSymlink naming %s, got %+v", dangling, is)
	}
	// A real leaf under a symlinked parent: flagged, naming the PARENT link.
	if is := byPath[underParent]; is.Kind != issueSymlink || is.Symlink != linkToDir {
		t.Errorf("symlinked parent: want issueSymlink naming %s, got %+v", linkToDir, is)
	}
	// A real directory with no symlinked component: no issue.
	if is, ok := byPath[realDir]; ok {
		t.Errorf("real directory should not be flagged, got %+v", is)
	}
}

// auditSecretInputs mirrors the scaffold's masked set. The token config file is
// surfaced for the bot_token_env recommendation only when the token is literally in
// it; masked-but-not-inline it is audited as a generic path; env-sourced it is not
// masked at all.
func TestAuditSecretInputsTokenSource(t *testing.T) {
	contains := func(ss []string, want string) bool {
		for _, s := range ss {
			if s == want {
				return true
			}
		}
		return false
	}

	// Inline bot_token: surfaced as the token file (→ bot_token_env recommendation),
	// not double-counted in the generic paths.
	c := &Config{ConfigPath: "/etc/bot.toml", tokenInFile: true}
	if paths, tf := c.auditSecretInputs(); tf != "/etc/bot.toml" || contains(paths, "/etc/bot.toml") {
		t.Errorf("inline token: want token file surfaced (not in paths), got tokenFile=%q paths=%v", tf, paths)
	}

	// Config file present but the token did NOT come from it (e.g. --bot-token flag):
	// the scaffold still masks the file defensively, so the audit classifies it as a
	// generic bare-file path — no bot_token_env recommendation (no inline token).
	c = &Config{ConfigPath: "/etc/bot.toml", tokenInFile: false}
	if paths, tf := c.auditSecretInputs(); tf != "" || !contains(paths, "/etc/bot.toml") {
		t.Errorf("defensively-masked config: want it in paths, no token file, got tokenFile=%q paths=%v", tf, paths)
	}

	// Token from an env var: nothing on disk, so the config file is not masked at all.
	c = &Config{ConfigPath: "/etc/bot.toml", BotTokenEnv: "TG_TOKEN"}
	if paths, tf := c.auditSecretInputs(); tf != "" || contains(paths, "/etc/bot.toml") {
		t.Errorf("env token: want config file NOT masked, got tokenFile=%q paths=%v", tf, paths)
	}
}

// Loading a config that defines bot_token literally sets tokenInFile, so the audit
// later recommends bot_token_env; a bot_token_env config does not.
func TestParseConfigTokenInFileFlag(t *testing.T) {
	inline := filepath.Join(t.TempDir(), "inline.toml")
	if err := os.WriteFile(inline, []byte("bot_token = \"12345:secret\"\nproject = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig([]string{"--config", inline})
	if err != nil {
		t.Fatal(err)
	}
	if !c.tokenInFile {
		t.Error("bot_token defined in file should set tokenInFile")
	}
	if _, tf := c.auditSecretInputs(); tf != c.ConfigPath {
		t.Errorf("inline token: audit should surface the config file, got %q", tf)
	}

	env := filepath.Join(t.TempDir(), "env.toml")
	if err := os.WriteFile(env, []byte("bot_token_env = \"MY_BOT_TOKEN\"\nproject = \"/tmp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = parseConfig([]string{"--config", env})
	if err != nil {
		t.Fatal(err)
	}
	if c.tokenInFile {
		t.Error("bot_token_env config should not set tokenInFile")
	}
}
