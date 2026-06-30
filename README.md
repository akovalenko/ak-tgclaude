# ak-tgclaude

A single-user **Telegram FAQ bot built on Claude Code**. One Go binary acts as a
long-lived **dispatcher** that receives Telegram updates and routes each one to a
**project-bound responder** — a headless `claude -p` session that answers from a
codebase and its notes — then sends the reply back to Telegram.

> Status: **design phase**. This README is the design of record; there is no
> working prototype yet. The code in this repo is a skeleton (command dispatch +
> config loading).

## Why one binary (multitool)

Everything is a single Go binary, selected by its first argument — no shell
sprawl, one thing to put on `PATH`:

| mode | where it runs | what it does |
|------|---------------|--------------|
| `dispatch` | host (trusted) | holds the bot token in memory, polls Telegram `getUpdates`, routes each update to a responder, watches the outbox spool and sends queued messages |
| `send` | inside the responder's sandbox | enqueues an outbound Telegram message by dropping a descriptor into the outbox spool (no token, no network) |
| `hook pretooluse` | as the responder's PreToolUse hook | gates the responder's tool calls (e.g. denies reads of the token file) |
| `deploy` | host, once | provisions the project tree, example config, and skills |

## Configuration

Configuration comes from a **TOML file**, **CLI flags**, or both — flags override
the file, the file overrides defaults (`flags > file > defaults`). A minimal
config (`bot.toml`, see `bot.toml.example`):

```toml
bot_token = "123456789:AA..."   # secret; kept in dispatcher memory, never in env
profile   = "qa"                # qa (read-only, default) | dev | ops (reserved)
project   = "~/code/myproject"  # the codebase consulted on (read-only under qa)
# runtime_base = ""             # base for the ephemeral cwd (default: $XDG_RUNTIME_DIR)
# state_dir    = ""             # durable state (default: $XDG_STATE_HOME/ak-tgclaude)
```

The same fields are CLI flags, so you can skip the file entirely for a quick
**"just try it"** run:

```sh
ak-tgclaude dispatch --bot-token 123:ABC --profile qa --project ~/code/myproject
```

- **Try-it vs production.** Prefer the **file** in production: a token in a file
  is protected by `sandbox.credentials.files` (deny-read in the responder's
  sandbox). A token on the **command line** is visible in the host's process list
  (`ps`) to other local processes — fine for a single-user "try it" run, not for
  a shared host. (It can **not** leak *into* the responder's sandbox; see
  [Why the CLI token is safe from the sandbox](#why-the-cli-token-is-safe-from-the-sandbox).)
- **Profiles** (`qa`/`dev`/`ops`) are a forward-looking dial: only `qa` (read-only)
  is implemented; `dev`/`ops` are reserved for a possible remote-development pivot,
  where the profile would widen the responder's access. The profile drives the
  responder's permissions and what its PreToolUse hook allows. `project`/`profile`
  are single for now; they may grow into a `[[project]]` array (per-project profile)
  later.

## Runtime layout (directories)

Three distinct locations, following the XDG split:

- **Ephemeral responder cwd** — a pseudo-random subdir created under
  `$XDG_RUNTIME_DIR` (private `0700` tmpfs), falling back to a temp dir. It holds
  only a **generated** `settings.json` (the responder's sandbox/hook config) and
  the responder skills, so it is disposable. The dispatcher **materializes** this
  scaffold at startup (the binary embeds the templates), then launches the
  responder there.
  - The pseudo-random subdir is created with `O_EXCL` (`os.MkdirTemp`), which
    defeats path-squatting even on a shared base: nobody can pre-create our dir as
    another user to block or hijack writes.
- **Config** — `$XDG_CONFIG_HOME/ak-tgclaude` (`~/.config/ak-tgclaude`).
- **Durable state** — `$XDG_STATE_HOME/ak-tgclaude` (`~/.local/state/ak-tgclaude`):
  the `chat→session` and `message→session` maps, which must survive restarts. It
  lives here, **not** in the launch cwd (state location must not depend on where
  the process was started from) nor in the ephemeral cwd (which is wiped).

## Token isolation

The Telegram **bot token** is the asset to protect: whoever holds it controls the
bot. The responder is a Claude Code instance executing model-chosen tool calls on
untrusted input (arbitrary Telegram messages), so it must never be able to read
the token.

- The token is held **only in the dispatcher's memory** (parsed from the TOML
  config, or read from `--bot-token` at startup). It is never placed in an
  environment variable and never written to a path the responder can read.
- The responder reaches Telegram only **indirectly**, by writing a descriptor to
  the **outbox spool**; the dispatcher is the only component that talks to the
  Telegram API.
- When the token comes from a **config file**, the binary registers that file in
  the responder's `sandbox.credentials.files` (`mode: "deny"`) so the sandbox
  denies any read of it — a backstop to the PreToolUse hook. (Requires Claude
  Code ≥ 2.1.187.)

### Why the CLI token is safe from the sandbox

A `--bot-token` on the dispatcher's command line lives in the dispatcher's
`argv`. A sandboxed sub-command **cannot** read it: the command sandbox runs in
its own **PID namespace**, so the dispatcher process is not visible from inside
it — `/proc/<dispatcher-pid>/cmdline` simply does not exist there (verified: a
sandboxed command sees only its own `/proc/1`, `/proc/2`; outside processes are
`No such file or directory`). The `ps`/`argv` exposure is therefore confined to
the host, never reaching the responder.

### Keeping ambient secrets out of the responder's shell

The command sandbox does **not** strip the environment: a sandboxed sub-command
inherits the full process env, including secrets like `ANTHROPIC_API_KEY` (and, in
some setups, proxy credentials). To keep those out of reach of a (possibly
prompt-injected) sub-command, the generated `settings.json` lists them under
`sandbox.credentials.envVars` (`mode: "deny"`), which **unsets** each variable
before every sandboxed command runs. Claude Code resolves its own API key at
session start, before this takes effect on tool sub-commands, so the responder's
model calls keep working while the shell never sees the secret.

> `sandbox.credentials.envVars` is preferred over a `settings.env` blank: it
> *unsets* the variable rather than setting it to an empty string. (`apiKeyHelper`
> is an alternative — the key is fed by a script and stays out of the tool env
> unless the script itself exports it.)

## Responder isolation

- Runs in its **own (ephemeral) cwd**, launched with `--setting-sources project`
  so only that project's generated `.claude/settings.json` is read —
  operator-global and local settings are excluded.
- The cwd is added to `sandbox.filesystem.allowWrite` so the responder may write
  there. Because settings paths are **not** environment-expanded, the binary
  writes the **literal** computed cwd path into the generated settings.json.
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
sanity-checks that it can find itself on `PATH`, warning if it cannot. The
PreToolUse hook is referenced by **bare PATH name** (`ak-tgclaude hook pretooluse`)
in the generated settings.json, never an absolute path.

## Approval UX

A motivating feature of the single-process design: the dispatcher can offer
inline-keyboard **yes/no approval buttons** in Telegram for gated actions.

## Repo layout

```
main.go            command dispatch (skeleton)
config.go          Config: TOML + CLI-flag resolution
bot.toml.example   example config
go.mod / go.sum
README.md          this design
deploy.sh          local dev build + install (gitignored, machine-specific)
```
