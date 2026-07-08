// Command ak-tgclaude is a single-user Telegram FAQ bot built on Claude Code.
// See README.md for the design. This file is a command-dispatch skeleton.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/akovalenko/ak-tgclaude/internal/store"
)

// version is reported by the `version` subcommand and advertised as the MCP
// server's serverInfo.version.
const version = "dev"

const usage = `ak-tgclaude — single-user Telegram FAQ bot (Claude Code dispatcher)

usage: ak-tgclaude <command> [args]

commands:
  dispatch   run the long-lived dispatcher: hold the bot token in memory,
             poll Telegram, route each update to a project-bound responder,
             and deliver its replies through the built-in MCP send tools
  hook       PreToolUse hook mode (e.g. "hook pretooluse"): gate the
             responder's tool calls (deny reads of the token file, ...)
  scaffold   materialize a responder cwd (generated settings.json) without
             running the dispatcher, to inspect it and run claude by hand
  audit      classify the configured sandbox deny-secrets by on-disk shape and
             report mask-leak windows (a missing path, a rename-replaceable bare
             file) plus whether the token should move to bot_token_env; read-only
  clear      drop every persisted chat->session binding (keeps the getUpdates
             offset). Reads the state dir from --config or the default
  recall     read the transcript store as groomed blocks (--dir SCOPE, then
             --msg N | --day/--since/--until). Used by the responder's tg-recall
             skill; read-only
  deploy     provision the project tree, example config, and skills.
             Assumes the binary is already on PATH (e.g. via "go install");
             it does NOT copy itself.
  version    print version and exit
`

// usageError marks a failure caused by bad flags/configuration rather than a
// runtime fault: main exits 2 for it (the flag-package convention), 1 otherwise.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// runRecall implements `ak-tgclaude recall --dir SCOPE <selector>` on top of
// the store package's transcript reader. The seam preserves the exit-code
// contract: everything ParseRecallArgs returns is a flag/selector mistake
// (usageError, exit 2), everything past parsing is a runtime fault (exit 1).
func runRecall(args []string) error {
	req, err := store.ParseRecallArgs(args)
	if err != nil {
		return usageError{err}
	}
	return store.Recall(os.Stdout, req)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	var err error
	switch cmd {
	case "dispatch":
		err = runDispatch(os.Args[2:])
	case "hook":
		err = runHook(os.Args[2:])
	case "scaffold":
		err = runScaffold(os.Args[2:])
	case "audit":
		err = runAudit(os.Args[2:])
	case "clear":
		err = runClear(os.Args[2:])
	case "recall":
		err = runRecall(os.Args[2:])
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "version":
		fmt.Println("ak-tgclaude " + version)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "ak-tgclaude: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	// The single exit point for every subcommand: run* report failures as errors
	// (a usageError for config/flag mistakes) instead of exiting in place.
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: %s: %v\n", cmd, err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
