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

## Dispatch loop & sessions

The dispatcher long-polls `getUpdates` and processes updates **sequentially**
(per-chat concurrency is a later optimization). For each message:

1. **`/clear`** drops the chat's session and acks — the next message starts fresh.
   This is the explicit "break the user↔session association" lever.
2. Otherwise it looks up the chat's session, creates a **per-invocation outbox
   directory**, and spawns the responder (`claude -p --agent … [--resume <id>]`)
   with `$AK_TGCLAUDE_OUTBOX` pointing at that directory and the message text on
   stdin. A drain bound to `Route{chat_id, reply_to=incoming message_id}` runs
   for the lifetime of that responder and delivers its messages (replying to the
   incoming one).
3. When the responder finishes, the session id it used (parsed from
   `--output-format json`) is bound to the chat, so the next message
   `--resume`s it.

**The per-invocation outbox dir is the route capability.** The dispatcher pins
the route in memory and binds it to the directory it handed this one responder;
the responder only writes its message, never names a chat — a confused-deputy is
closed by construction, no separate uid needed. (This is the route-binding
decision the spool transport deliberately left open: a private dir per
invocation, rather than a shared spool plus a per-message token.)

Session ids are **not** derived from the chat id — Claude Code mints a fresh one
per new conversation and the dispatcher captures it, so `/clear` can truly sever
the association. The `chat→session` map and the poll offset are the dispatcher's
durable state (`$XDG_STATE_HOME/ak-tgclaude/sessions.json`).

> Replying to an old bot message to **resurrect** its (since-cleared) session —
> a `message→session` map keyed by the sent `message_id` — is a planned follow-up
> (each send already returns its `message_id`).

### Smoke-testing the Telegram path (`--responder stub`)

`dispatch --responder stub` swaps the model for a stub that replies a fixed line
("i am here") to every message, dropped through the **real** outbox — so the full
Telegram I/O path runs (getUpdates → route → outbox → drain → `sendMessage` with
reply threading) without `claude` or a provisioned scaffold. It needs only a
token:

```sh
ak-tgclaude dispatch --responder stub --bot-token 123:ABC
```

Use it to verify connectivity, the bot token, long-polling, and reply routing
end to end before wiring the real responder.

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

### The descriptor

Each dropped file is one **descriptor** — a single outbound action, JSON. It
carries the **semantic** message (what to say), never the **route** (where): the
dispatcher pins `chat_id`/`reply_to` in-process and ignores anything a responder
might add. A `kind` discriminator plus a `v` schema version keep it extensible —
a new kind (`photo`, an inline-keyboard for the approval UX) or field is
non-breaking, since a reader switches on `kind` and ignores fields it does not
know.

```jsonc
{ "v": 1, "kind": "text",     "text": "…", "format": "plain|html", "silent": false }
{ "v": 1, "kind": "code",     "code": "…", "language": "go", "caption": "…" }
{ "v": 1, "kind": "document", "path": "/abs/file.pdf", "filename": "report.pdf", "caption": "…" }
```

- **`text`** — a message. `format: "plain"` (default, shown verbatim) or `"html"`
  (Telegram `parse_mode=HTML`; the responder supplies valid, escaped HTML — its
  full inline-formatting escape hatch).
- **`code`** — a preformatted block with an optional `language`. The dispatcher
  renders it as `<pre><code class="language-LANG">…</code></pre>` (escaping the
  body for you) and **spills to a document** when it exceeds Telegram's size
  limit.
- **`document`** — a file attachment. `path` is **absolute** so it survives the
  responder's ephemeral cwd; the dispatcher uploads it before that cwd is torn
  down.

Rendering to Telegram HTML and the oversize-spill policy live in the
**dispatcher**, so `send` only serializes intent.

### The `send` surface

`send` runs inside the responder sandbox and drops one descriptor per call into
`$AK_TGCLAUDE_OUTBOX` (the dispatcher sets this to the directory bound to the
invocation's route). The body is a positional argument, or stdin (`-`/omitted)
for large content:

```sh
ak-tgclaude send text [--html] [--silent] [body|-]
ak-tgclaude send code [--lang go] [--caption main.go] [--silent] [body|-]
ak-tgclaude send doc  [--filename report.pdf] [--caption "…"] [--silent] <path>
```

A responder may call `send` several times to emit multiple messages for one
update (the rich agent facade — text, code, attachments, "think and send more"
— is preserved).

### Dispatcher-side delivery (drain)

The dispatcher runs one **drain** per invocation's outbox: it watches the
directory (fsnotify) and, on each drop, sends the descriptors to Telegram **in
drop order** (filenames sort by drop time), deleting each only after a successful
send. It is the sole drainer of that directory, so there is no concurrent
send/remove race. The route (`chat_id`/`reply_to`) is the dispatcher's, bound to
the outbox — never taken from the descriptor.

- **Catch-up + final flush.** It first sends whatever is already present (the
  responder may write before the watcher registers), then streams new drops; when
  the responder exits it does a final flush so nothing is left behind.
- **Rendering lives here.** `text` goes out as `sendMessage` (plain, or
  `parse_mode=HTML`); `code` is wrapped in `<pre><code class="language-…">` with
  the body HTML-escaped; a message over Telegram's 4096-char limit **spills to a
  document** (the raw, unwrapped payload). `document` is a `sendDocument` upload.
- **Ordering and failures.** Sends are sequential per outbox. A transient send
  failure stops the pass — leaving that descriptor and everything after it — so a
  retry preserves order (head-of-line). An unparseable descriptor is moved to
  `<outbox>/bad/` and skipped, so junk never blocks the queue.

Each send returns the Telegram `message_id`, which the dispatcher will later map
back to the responder's session for **reply-resurrection** (replying to an old
bot message revives its `--resume` session).

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
outbox.go          outbound descriptor model + atomic spool drop
send.go            `send` subcommand (text / code / document)
render.go          descriptor -> Telegram text/parse_mode, code wrapping, spill
telegram.go        Telegram Bot API client (getUpdates / sendMessage / sendDocument)
drain.go           per-invocation outbox drain (fsnotify watch -> send -> ack)
session.go         durable state: poll offset + chat->session map
responder.go       Responder interface + `claude -p` spawn
dispatch.go        the dispatch loop: poll -> route -> respond -> deliver
bot.toml.example   example config
go.mod / go.sum
README.md          this design
deploy.sh          local dev build + install (gitignored, machine-specific)
```
