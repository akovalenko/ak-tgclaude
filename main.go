// Command ak-tgclaude is a single-user Telegram FAQ bot built on Claude Code.
// See README.md for the design. This file is a command-dispatch skeleton.
package main

import (
	"fmt"
	"os"
)

const usage = `ak-tgclaude — single-user Telegram FAQ bot (Claude Code dispatcher)

usage: ak-tgclaude <command> [args]

commands:
  dispatch   run the long-lived dispatcher: hold the bot token in memory,
             poll Telegram, route each update to a project-bound responder,
             and flush the outbox spool to Telegram
  send       (run inside the responder sandbox) enqueue an outbound message
             by dropping a descriptor into the outbox spool
  hook       PreToolUse hook mode (e.g. "hook pretooluse"): gate the
             responder's tool calls (deny reads of the token file, ...)
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
	case "send":
		runSend(os.Args[2:])
	case "hook", "deploy":
		todo(cmd)
	case "version":
		fmt.Println("ak-tgclaude (dev)")
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "ak-tgclaude: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func todo(cmd string) {
	fmt.Fprintf(os.Stderr, "ak-tgclaude: %s: not implemented yet\n", cmd)
	os.Exit(1)
}

// runDispatch loads configuration and (eventually) runs the dispatcher loop.
func runDispatch(args []string) {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("config loaded: profile=%s project=%s state_dir=%s bot_token=%s\n",
		cfg.Profile, cfg.Project, cfg.StateDir, redact(cfg.BotToken))
	fmt.Fprintln(os.Stderr, "ak-tgclaude: dispatch: dispatcher loop not implemented yet")
	os.Exit(1)
}
