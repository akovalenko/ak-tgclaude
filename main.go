// Command ak-tgclaude is a single-user Telegram FAQ bot built on Claude Code.
// See README.md for the design. This file is a command-dispatch skeleton.
package main

import (
	"fmt"
	"os"
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
  clear      drop every persisted chat->session binding (keeps the getUpdates
             offset). Reads the state dir from --config or the default
  deploy     provision the project tree, example config, and skills.
             Assumes the binary is already on PATH (e.g. via "go install");
             it does NOT copy itself.
  version    print version and exit
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch cmd := os.Args[1]; cmd {
	case "dispatch":
		runDispatch(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "scaffold":
		runScaffold(os.Args[2:])
	case "clear":
		runClear(os.Args[2:])
	case "deploy":
		runDeploy(os.Args[2:])
	case "version":
		fmt.Println("ak-tgclaude " + version)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "ak-tgclaude: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
