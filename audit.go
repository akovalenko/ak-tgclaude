package main

import (
	"fmt"
	"io"
	"log"
	"os"
)

// secretIssueKind classifies an auditSecrets finding: which sandbox-mask leak
// window a configured deny-secret is exposed to, or a weaker-than-necessary token
// source. The sandbox masks a secret only as it exists at command start — see the
// README "Sandbox masking is a start-of-command snapshot" section.
type secretIssueKind int

const (
	// issueMissing: the path does not exist (or cannot be stat'd) at audit time. A
	// deny of a nonexistent path is a silent no-op — bwrap masks only what exists at
	// namespace setup — so a secret created there AFTER a long-running sandboxed
	// command starts is read in the clear (leak window 1). Pre-create the path (as a
	// directory) so the mask is in place before the responder runs.
	issueMissing secretIssueKind = iota
	// issueBareFile: the path is a plain FILE, not a directory. A file-level mask is
	// pinned to the file's inode; replacing the file by rename (atomic
	// write-temp+rename, e.g. a credential refresh) slides a fresh inode in under the
	// now-orphaned mask and the new file is read in the clear (leak window 2). Keep
	// the secret inside a whole-directory deny instead — a directory mask covers
	// every name under it, rename included.
	issueBareFile
	// issueTokenInFile: the bot token is stored literally in a config file
	// (bot_token). That file is a bare file (so also window 2), and env/flag sourcing
	// keeps the token off disk entirely — prefer bot_token_env.
	issueTokenInFile
)

// secretIssue is one auditSecrets finding: the offending path and its kind.
type secretIssue struct {
	Path string
	Kind secretIssueKind
}

// warning renders a one-line operator-facing message for the issue — the same text
// the `audit` subcommand prints and the dispatcher startup check logs.
func (i secretIssue) warning() string {
	switch i.Kind {
	case issueMissing:
		return fmt.Sprintf("%s does not exist (or is unreadable): a deny of a missing path is a silent no-op, "+
			"so a secret created there after the responder starts would go unmasked (leak window 1) — pre-create it as a directory", i.Path)
	case issueBareFile:
		return fmt.Sprintf("%s is a bare file: a file-level mask is bypassed when the file is replaced by rename "+
			"(e.g. a credential refresh), leaking the new contents (leak window 2) — keep the secret inside a whole-directory deny instead", i.Path)
	case issueTokenInFile:
		return fmt.Sprintf("the bot token is stored literally in %s: prefer bot_token_env (an env var, read then unset at startup), "+
			"which keeps the token off disk and needs no bypassable file deny", i.Path)
	default:
		return fmt.Sprintf("%s: unknown issue", i.Path)
	}
}

// auditSecrets classifies each configured deny-secret path by its on-disk shape and
// returns the leak windows it is exposed to. paths are the resolved absolute secret
// paths guarded at both layers (host secret dirs + operator deny_reads). tokenFile,
// when non-empty, is the config file the bot token lives in literally — it earns an
// issueTokenInFile note steering to bot_token_env (and is NOT also in paths, so it
// yields exactly that one finding, not a redundant window-2 note). A directory path
// is robust (its mask survives rename and covers later-created names) and yields no
// issue; a missing path is window 1, a bare file is window 2. Statting the
// filesystem is the only impurity — the classification is otherwise a pure function
// of its inputs, so a test drives it with temp dirs/files.
func auditSecrets(paths []string, tokenFile string) []secretIssue {
	var issues []secretIssue
	for _, p := range paths {
		info, err := os.Stat(p)
		switch {
		case err != nil:
			// Missing, or an unreadable/broken path we cannot confirm is a safe
			// directory: treat conservatively as window 1.
			issues = append(issues, secretIssue{Path: p, Kind: issueMissing})
		case !info.IsDir():
			issues = append(issues, secretIssue{Path: p, Kind: issueBareFile})
		}
	}
	if tokenFile != "" {
		issues = append(issues, secretIssue{Path: tokenFile, Kind: issueTokenInFile})
	}
	return issues
}

// auditSecretInputs returns the resolved secret paths to audit plus the inline token
// file (or ""). It mirrors the EXACT masked set the scaffold produces — host secret
// dirs (~/.ssh, ~/.claude) + operator deny_reads + the token config file — so the
// audit and the running bot can never disagree about what is (and isn't) masked. The
// token file is derived from scaffoldParams itself (ConfigPath unless the token rides
// an env var, which puts nothing on disk), not re-implemented here, so a future
// change to that rule carries into the audit automatically. Run after parseConfig, so
// c.DenyRead is resolved absolute (hostSecretHookDeny resolves the host set itself).
//
// The token config file is split out only when the token is stored literally in it
// (tokenInFile): it then earns the bot_token_env recommendation (issueTokenInFile),
// which subsumes the generic window-2 note. When the file is masked defensively
// though the token came from --bot-token, it is audited like any other bare-file
// secret (window 2) — no env recommendation, since there is no inline token to move.
func (c *Config) auditSecretInputs() (paths []string, tokenFile string) {
	paths = append(paths, hostSecretHookDeny()...)
	paths = append(paths, c.DenyRead...)
	// cacheDir/outboxRoot do not affect TokenFile, so pass "" — we only want the
	// scaffold's masked-token-file decision, the single source of truth.
	masked := c.scaffoldParams("", "").TokenFile
	switch {
	case masked != "" && c.tokenInFile:
		tokenFile = masked
	case masked != "":
		paths = append(paths, masked)
	}
	return paths, tokenFile
}

// logSecretAudit runs the audit for a resolved config and logs each finding as a
// non-fatal warning. The dispatcher calls it at startup so a live bot flags a weak
// secret setup in its log without refusing to start; the `audit` subcommand prints
// the same findings (plus a clean-bill line) to stdout instead.
func (c *Config) logSecretAudit() {
	paths, tokenFile := c.auditSecretInputs()
	for _, is := range auditSecrets(paths, tokenFile) {
		log.Printf("ak-tgclaude: secret audit: %s", is.warning())
	}
}

// writeAuditReport prints the audited paths and every finding to w. With no issues
// it prints a clean-bill line; otherwise one line per issue. Shared by the `audit`
// subcommand (stdout) and the scaffold subcommand's inspection output.
func writeAuditReport(w io.Writer, paths []string, tokenFile string, issues []secretIssue) {
	fmt.Fprintln(w, "ak-tgclaude: auditing sandbox deny-secrets for mask-leak windows")
	for _, p := range paths {
		fmt.Fprintf(w, "  audited: %s\n", p)
	}
	if tokenFile != "" {
		fmt.Fprintf(w, "  token source: config file %s (inline bot_token)\n", tokenFile)
	}
	if len(issues) == 0 {
		fmt.Fprintln(w, "OK: every deny-secret is a whole directory that exists — no mask-leak window.")
		return
	}
	fmt.Fprintf(w, "%d issue(s):\n", len(issues))
	for _, is := range issues {
		fmt.Fprintf(w, "  - %s\n", is.warning())
	}
}

// runAudit classifies the configured deny-secrets by their on-disk shape and prints
// the leak windows each is exposed to (missing => window 1, bare file => window 2),
// plus a bot_token_env recommendation when the token lives literally in a config
// file. It reads config the same way the dispatcher does (parseConfig — no token
// required) but never starts the bot. Exit is 0 whether or not issues are found —
// the report is the product, not a pass/fail gate.
func runAudit(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return usageError{err}
	}
	paths, tokenFile := cfg.auditSecretInputs()
	writeAuditReport(os.Stdout, paths, tokenFile, auditSecrets(paths, tokenFile))
	return nil
}
