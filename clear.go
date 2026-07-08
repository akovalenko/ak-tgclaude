package main

import (
	"fmt"
	"os"

	"github.com/akovalenko/ak-tgclaude/internal/store"
)

// runClear implements `ak-tgclaude clear`: drop every persisted chat→session
// binding from the state dir, keeping the getUpdates offset so the dispatcher
// does not reprocess the backlog on its next start. The state dir is resolved
// from the config file (--config) or the default; no bot token is needed. This
// is the one-shot alternative to running the dispatcher with ephemeral_sessions.
// Failures are returned for main to report and exit-code.
func runClear(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return usageError{err}
	}
	sessions, err := store.LoadSessions(cfg.SessionDir(), false)
	if err != nil {
		return err
	}
	for _, p := range sessions.Outboxes() {
		if err := os.RemoveAll(p); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: clear: removing outbox %s: %v\n", p, err)
		}
	}
	n, err := sessions.ClearAll()
	if err != nil {
		return err
	}
	fmt.Printf("cleared %d chat→session binding(s) in %s\n", n, cfg.SessionDir())
	return nil
}
