package main

import (
	"fmt"
	"os"
)

// runClear implements `ak-tgclaude clear`: drop every persisted chat→session
// binding from the state dir, keeping the getUpdates offset so the dispatcher
// does not reprocess the backlog on its next start. The state dir is resolved
// from the config file (--config) or the default; no bot token is needed. This
// is the one-shot alternative to running the dispatcher with ephemeral_sessions.
func runClear(args []string) {
	cfg, err := parseConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: clear: %v\n", err)
		os.Exit(2)
	}
	store, err := LoadSessionStore(cfg.SessionDir(), false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: clear: %v\n", err)
		os.Exit(1)
	}
	n, err := store.ClearAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: clear: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("cleared %d chat→session binding(s) in %s\n", n, cfg.SessionDir())
}
