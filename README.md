# ak-tgclaude

A single-user **Telegram FAQ bot built on Claude Code**. One Go binary acts as a
long-lived **dispatcher** that receives Telegram updates and routes each one to a
**project-bound responder** — a headless `claude -p` session that answers from a
codebase and its notes — then sends the reply back to Telegram.

> Status: **design phase**. This README is the design of record; there is no
> working prototype yet. The code in this repo is a skeleton.

## Why one binary (multitool)

Everything is a single Go binary, selected by its first argument — no shell
sprawl, one thing to put on `PATH`:

| mode | where it runs | what it does |
|------|---------------|--------------|
| `dispatch` | host (trusted) | holds the bot token in memory, polls Telegram `getUpdates`, routes each update to a responder, watches the outbox spool and sends queued messages |
| `send` | inside the responder's sandbox | enqueues an outbound Telegram message by dropping a descriptor into the outbox spool (no token, no network) |
| `hook pretooluse` | as the responder's PreToolUse hook | gates the responder's tool calls (e.g. denies reads of the token file) |
| `deploy` | host, once | provisions the project tree, example config, and skills |

## Token isolation

The Telegram **bot token** is the asset to protect: whoever holds it controls the
bot. The responder is a Claude Code instance executing model-chosen tool calls on
untrusted input (arbitrary Telegram messages), so it must never be able to read
the token.

- The token is parsed **into the dispatcher's memory** from a config file
  (`bot.json` / `bot.toml`) at startup. It is never placed in an environment
  variable and never written to a path the responder can read.
- The responder reaches Telegram only **indirectly**, by writing a descriptor to
  the **outbox spool**; the dispatcher is the only component that talks to the
  Telegram API.
- Defense in depth: a **PreToolUse hook** (this same binary) denies the responder
  any read of the token file, and the sandbox can additionally deny-read it.

### Keeping ambient secrets out of the responder's shell

The command sandbox does **not** strip the environment: a sandboxed sub-command
sees the full inherited env, including secrets such as `ANTHROPIC_API_KEY`. To
keep those out of reach of a (possibly prompt-injected) sub-command, the responder
is launched with a project `settings.json` whose `env` block blanks the secret
variables. Claude Code resolves its own API key at session start, *before* that
`env` is applied to tool sub-commands, so blanking it does not break the
responder's own model calls — it only hides the value from the shell.

> To be confirmed empirically before being relied on as a security boundary,
> together with whether a headless `claude -p` inherits or clears the parent
> environment in the first place.

## Responder isolation

- Runs in its **own working directory** (the project root), launched with
  `--setting-sources project` so only that project's `.claude/settings.json` is
  read — operator-global and local settings are excluded.
- Uses an **isolated module/tool cache** so its activity does not touch the host's.

## Outbound transport: an outbox directory

The responder hands outbound messages to the dispatcher through a **spool
directory**, not a pipe:

- **Durable / queued** — a dropped file survives a dispatcher restart; a pipe with
  no reader loses data and blocks the writer.
- **Decoupled** — the writer never blocks on the reader.
- **Multi-instance** — unique filenames + atomic rename; no interleaving, no
  single-consumer bottleneck.
- **Crash-safe and inspectable.**

The dispatcher watches the directory (fsnotify) for sub-millisecond pickup, so the
"real-time" edge of a FIFO is not worth its fragility. Each message is dropped via
a temp-file-plus-atomic-rename, so the watcher never sees a partial write.

The **return / private channel** (dispatcher → a specific responder instance) is a
separate concern from this outbound spool and may use a per-instance channel.

## Install & deploy

The binary is distributed the normal Go way (`go install`), so by the time you run
it, it is already on `PATH`. The `deploy` subcommand therefore **does not copy
itself** — it provisions everything else (project root, example config, skills) and
sanity-checks that it can find itself on `PATH`, warning if it cannot.

Layout is parametrized (no hardcoded paths). Example:

- binary — wherever `go install` placed it (on `PATH`)
- project root (responder cwd; holds `settings.json` and skills) — e.g. `~/qa`
- example config — e.g. `~/qa-conf`

## Approval UX

A motivating feature of the single-process design: the dispatcher can offer
inline-keyboard **yes/no approval buttons** in Telegram for gated actions.

## Repo layout

```
main.go     command dispatch (skeleton)
go.mod
README.md   this design
deploy.sh   local dev build + install (gitignored, machine-specific)
```
