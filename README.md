# ak-tgclaude

A single-user **Telegram FAQ bot built on Claude Code**. One Go binary acts as a
long-lived **dispatcher** that receives Telegram updates and routes each one to a
**project-bound responder** — a headless `claude -p` session that answers from a
codebase and its notes — then sends the reply back to Telegram.

> Status: **implemented, under active development**. The dispatcher, the
> project-bound responder (`claude -p`, plus a `stub` for Telegram-I/O smoke
> tests), the MCP-over-HTTP outbound transport with synchronous delivery feedback,
> and the supporting subcommands (`scaffold` / `audit` / `clear` / `recall` /
> `deploy` / `hook`) are built and unit-tested. This README remains the design of
> record; the non-QA profiles are reserved but not wired yet.

## Contents

- [Why one binary (multitool)](#why-one-binary-multitool)
- [Configuration](#configuration)
- [Runtime layout (directories)](#runtime-layout-directories)
- [Dispatch loop & sessions](#dispatch-loop--sessions)
  - [Smoke-testing the Telegram path (`--responder stub`)](#smoke-testing-the-telegram-path---responder-stub)
- [Transcripts & recall](#transcripts--recall)
- [Access control](#access-control)
- [Groups](#groups)
- [Token isolation](#token-isolation) — **security**
  - [Why the CLI token is safe from the sandbox](#why-the-cli-token-is-safe-from-the-sandbox)
  - [Keeping ambient secrets out of the responder's shell](#keeping-ambient-secrets-out-of-the-responders-shell)
  - [Host secrets beyond the bot token](#host-secrets-beyond-the-bot-token)
  - [Deployment: a shared host with live secrets](#deployment-a-shared-host-with-live-secrets)
  - [Sandbox masking is a start-of-command snapshot — two leak windows](#sandbox-masking-is-a-start-of-command-snapshot--two-leak-windows)
- [Responder (agent + emission skill)](#responder-agent--emission-skill)
  - [Policy (persona)](#policy-persona)
  - [Wiring domain skills](#wiring-domain-skills)
  - [Generic skills & agents (on-demand, not preloaded)](#generic-skills--agents-on-demand-not-preloaded)
- [Responder scaffold (generated settings.json)](#responder-scaffold-generated-settingsjson)
  - [Per-invocation isolation (write and read)](#per-invocation-isolation-write-and-read)
  - [Static workdir vs ephemeral cwd, and `scaffold`](#static-workdir-vs-ephemeral-cwd-and-scaffold)
- [Responder isolation](#responder-isolation)
- [Outbound transport: an MCP server](#outbound-transport-an-mcp-server)
  - [The route capability is the token, not a directory](#the-route-capability-is-the-token-not-a-directory)
  - [The tools](#the-tools)
  - [Document path confinement](#document-path-confinement)
  - [Delivery and errors](#delivery-and-errors)
- [Install & deploy](#install--deploy)
- [Approval UX](#approval-ux)
- [Repo layout](#repo-layout)

## Why one binary (multitool)

Everything is a single Go binary, selected by its first argument — no shell
sprawl, one thing to put on `PATH`:

| mode | where it runs | what it does |
|------|---------------|--------------|
| `dispatch` | host (trusted) | holds the bot token in memory, polls Telegram `getUpdates`, routes each update to a responder, and runs the MCP server that delivers the responder's replies to Telegram |
| `hook pretooluse` | as the responder's PreToolUse hook | gates the responder's tool calls (e.g. denies reads of the token file) |
| `scaffold` | host | materializes a responder `workdir/project` (generated settings.json) without running the dispatcher, to inspect it and run `claude` by hand |
| `audit` | host | classifies the configured sandbox deny-secrets by on-disk shape and reports mask-leak windows (a missing path, a rename-replaceable bare file) plus whether the token should move to `bot_token_env`; read-only, never starts the bot |
| `clear` | host | drops every persisted chat→session binding (keeps the getUpdates offset); reads the state dir from `--config` or the default |
| `recall` | responder (sandboxed) | reads the transcript store as groomed blocks (`--dir SCOPE`, then `--msg N` or `--day`/`--since`/`--until`); backs the `tg-recall` skill, read-only |
| `deploy` | host, once | writes an example config and (with `--workdir`) provisions the static workdir + marks it trusted |

## Configuration

Configuration comes from a **TOML file**, **CLI flags**, or both — flags override
the file, the file overrides defaults (`flags > file > defaults`). A minimal
config (`bot.toml`, see `bot.toml.example`):

```toml
bot_token = "123456789:AA..."   # secret; kept in dispatcher memory, never in env. Inline = secret AT REST — prefer bot_token_env below
# bot_token_env = "TG_BOT_TOKEN" # PREFERRED: read the token from this env var at startup, then unset it (nothing on disk). Or --bot-token (off disk; ps-visible)
profile   = "qa"                # qa (read-only, default) | dev | ops (reserved)
project   = "~/code/myproject"  # the codebase consulted on (read-only under qa)
# wire_skills = ["~/lib/eputs-qa-knowledge"]  # domain skill(s) preloaded into the responder
# deny_reads = ["~/code/myproject/secrets.env"]  # extra paths the responder must never read
# deny_envs  = ["MY_SECRET"]     # extra env-var names to scrub (ANTHROPIC keys + CLAUDE_CODE_OAUTH_TOKEN always scrubbed)
# allow_domains = ["api.github.com"]  # extra sandbox egress domains, on top of the Go-build defaults
# claude_args = ["--model", "opus", "--effort", "high"]  # extra raw `claude -p` flags (ak-tgclaude-owned flags rejected)
# allow_silent = false          # true DISABLES the delivery guard (below); default false = guard on
# undelivered_text = "Sorry, I could not answer that."  # fallback reply if the guard's re-prompt still sent nothing
# overflow = "spill"           # oversized reply: "spill" (default; whole answer as one .md doc) | "error" (make the model shorten). Code always spills to .md
# upload_command = "~/uploaders/rsync-upload.sh"  # large-file fallback: docs over ~40 MB uploaded and sent as a link (below; see examples/)
# tools = ["Agent", "WebFetch(domain:*.github.com)"]  # grant EXTRA tools: bare name→frontmatter, full spec→--allowedTools; sharp knob — see below
# runtime_base = ""             # base for the ephemeral cwd (default: $XDG_RUNTIME_DIR)
# outbox_root  = ""             # where per-chat outboxes live; MUST be outside the responder cwd (default $workdir/outbox, sibling of project). Point at a size-capped tmpfs to bound disk
# state_dir    = ""             # durable state (default: $XDG_STATE_HOME/ak-tgclaude)
# transcripts = false           # per-chat transcript store + tg-recall (default OFF; records users' text to disk — see below)
# transcript_dir = ""           # override the store root (default <state>/transcripts; must be outside the responder outbox)
# owner_reads_all = true        # with transcripts on, the owner reads the WHOLE store (cross-chat); false = owner scoped like any user
```

The same fields are CLI flags, so you can skip the file entirely for a quick
**"just try it"** run:

```sh
ak-tgclaude dispatch --bot-token 123:ABC --profile qa --project ~/code/myproject
```

- **Try-it vs production.** In production prefer **`bot_token_env`** — an env var the
  dispatcher reads then unsets at startup, so the token never touches disk (see
  [Token isolation](#token-isolation)). An inline **`bot_token`** in the file is a
  secret *at rest* whose sandbox deny a rename can bypass
  ([window 2](#sandbox-masking-is-a-start-of-command-snapshot--two-leak-windows)),
  and `--bot-token` on the **command line** is visible in the host process list
  (`ps`) to other local processes — fine for a single-user "try it" run, not for a
  shared host. (Neither the flag nor the file token can leak *into* the responder's
  sandbox; see
  [Why the CLI token is safe from the sandbox](#why-the-cli-token-is-safe-from-the-sandbox).)
- **Profiles** (`qa`/`dev`/`ops`) are a forward-looking dial: only `qa` (read-only)
  is implemented; `dev`/`ops` are reserved for a possible remote-development pivot,
  where the profile would widen the responder's access. The profile drives the
  responder's permissions and what its PreToolUse hook allows. `project`/`profile`
  are single for now; they may grow into a `[[project]]` array (per-project profile)
  later.
- **Paths.** Every path field (`project`, `workdir`, `wire_skills`, `add_skills`,
  `add_agents`, `deny_reads`, `state_dir`, `runtime_base`, `outbox_root`,
  `--config`) takes a leading `~` and is made
  **absolute against the dispatcher's launch cwd**. The responder consumes them
  from a different cwd (the scaffold dir), so they are resolved once, up front —
  a relative path means "relative to where I launched the bot", never the
  responder's cwd. Paths must be **literal**: a glob metacharacter (`* ? [ ] \`)
  or control character is **rejected at startup**, because the sandbox filesystem
  rules glob-match and would otherwise silently protect/expose the wrong files
  (spaces and quotes are fine). Rename or symlink around such a path.
- **Extra `claude` flags (`claude_args`, repeatable `--claude-arg`, or the
  one-string `--claude-args`).** Raw arguments appended verbatim to the responder's
  `claude -p` — e.g. `--model`, `--effort`, `--verbose` — so any current or future
  claude flag works without a dedicated knob. Three additive sources: the TOML
  `claude_args` list, repeatable `--claude-arg` (one token each), and the CLI
  convenience `--claude-args "--model opus --effort high"` (one whitespace-split
  string; a flag value with a space needs `--claude-arg`/`claude_args` instead). The flags
  ak-tgclaude **owns** are rejected **at startup** with a clear error, rather than
  allowed to silently override what the design pins: the security gate
  (`--permission-mode`, `--setting-sources`, the skip-permissions escapes), the MCP
  transport (`--mcp-config`, `--strict-mcp-config`, `--allowedTools`), the
  per-invocation `--settings`, the session flags the dispatcher manages (`--agent`,
  `--resume`/`-r`, `--continue`/`-c`), and the print/format flags it parses
  (`-p`/`--print`, `--output-format`, `--input-format`). claude's duplicate-flag
  precedence is undocumented, so an override is refused rather than trusted to win.
  Everything else passes through — you can break the bot's *behavior*, but not its
  sandbox.
- **Delivery guard (on by default; `allow_silent` / `--allow-silent` disables it).**
  The responder delivers an answer **only** by calling a send tool — its final text
  is just a status signal and is discarded, never sent. A weaker model sometimes
  ends a turn without calling one, dumping its answer into that discarded text, so
  the user gets nothing. When guarded, the dispatcher notices a turn delivered
  **zero** messages and **re-prompts the same session once** to actually send; if it
  still sends nothing and `undelivered_text` is set, that fixed line goes out as a
  last resort (empty ⇒ the guard only re-prompts and logs). Set `allow_silent = true`
  (or `--allow-silent`) only if a no-send turn is legitimate for your bot. **Progress
  notes don't count:** a send tagged `progress: true` (an "along the way" status line)
  is delivered but excluded from the tally, so a responder that narrates slow work
  before answering doesn't blind the guard — only a real (untagged) send clears it.
- **Long answers (`overflow` / `--overflow`).** A reply too long for one Telegram
  message (4096 UTF-16 units) is first **split** at paragraph breaks — newlines with
  no HTML element open — into up to a few messages, each independently valid HTML.
  When it will not split (one oversized block, or code), the policy decides: `spill`
  (default) sends the **whole** answer as a single Markdown `.md` document, which
  Telegram renders in-app, with a short caption; `error` returns a tool error so the
  model shortens or restructures it. **Code always spills to `.md`** regardless (it
  cannot be "made shorter"). A split reply is recorded once in the transcript at its
  anchor message, with the follow-up pieces stubbed to it (`part_of`; see recall).
- **Extra tools (`tools`, repeatable `--tool`).** Grant the responder additional
  tools. Each value is split across the two grants that must change together: its
  bare NAME goes into the agent's `tools:` frontmatter (availability) and the value
  VERBATIM goes into `--allowedTools` (permission) — one knob keeps them from
  drifting. So a scoped spec like `WebFetch(domain:*.github.com)` grants bare
  `WebFetch` availability plus that exact domain scope, and two scopes of the same
  verb (`WebFetch(domain:a)`, `WebFetch(domain:b)`) collapse to a single `WebFetch`
  in the frontmatter while both scopes ride `--allowedTools`. Quote the spec so your
  shell leaves the parens/asterisks alone (`--tool 'WebFetch(domain:*.github.com)'`);
  the value reaches `claude` literally (an `exec.Command` arg, never shell-expanded).
  Values are tool names, scoped specs, or MCP patterns (`Agent`,
  `WebFetch(domain:X)`, `mcp__srv__*`). **Sharp knob:** the sandbox still confines
  Bash and the PreToolUse hook still gates the file tools, but a tool the hook does
  *not* gate (`WebFetch`, `Agent`, …) genuinely widens what the responder can reach
  — grant deliberately. Extras are merged with the always-present tg send tools and
  deduped.

## Runtime layout (directories)

Distinct locations, following the XDG split:

- **Ephemeral responder cwd** — a pseudo-random subdir created under
  `$XDG_RUNTIME_DIR` (private `0700` tmpfs), falling back to a temp dir. It holds
  only a **generated** `settings.json` (the responder's sandbox/hook config) and
  the responder skills, so it is disposable. The dispatcher **materializes** this
  scaffold at startup (the binary embeds the templates), then launches the
  responder there. The cwd is **write-denied** in the sandbox (`denyWrite`), so the
  responder cannot mutate its own scaffold — see [Responder isolation](#responder-isolation).
  - The pseudo-random subdir is created with `O_EXCL` (`os.MkdirTemp`), which
    defeats path-squatting even on a shared base: nobody can pre-create our dir as
    another user to block or hijack writes.
- **Outbox root** — where per-chat outboxes (the responder's writable scratch:
  attachments, downloaded incoming media, build outputs) live. Deliberately kept
  **outside** the responder cwd, since the cwd is write-denied: a sibling
  `$workdir/outbox` with a workdir, a disposable temp beside the ephemeral cwd
  otherwise, or an explicit `outbox_root` (e.g. a size-capped tmpfs mount to bound a
  chat's disk use). Never a subdir of cwd — startup rejects an `outbox_root` under
  it, because the project `denyWrite` would make it unwritable.
- **Config** — `$XDG_CONFIG_HOME/ak-tgclaude` (`~/.config/ak-tgclaude`).
- **Durable state** — the `chat→session` and `message→session` maps (in
  `sessions.json`, alongside the getUpdates offset), which must survive restarts.
  Default `$XDG_STATE_HOME/ak-tgclaude` (`~/.local/state/ak-tgclaude`); with a
  `workdir` it moves to `$workdir/state`. Either way it is **not** in the launch cwd
  (state must not depend on where the process started) nor in the ephemeral cwd
  (which is wiped). The Go build cache stays under `state_dir` even with a workdir,
  so it is shared across bots rather than duplicated per-workdir.

## Dispatch loop & sessions

The dispatcher long-polls `getUpdates` and dispatches each update to a **per-chat
worker**: different chats are handled **concurrently** (bounded by
`max_concurrent`), while updates within one chat are **serialized** (so they
never race on the same `--resume` session). For each message:

1. **Commands the dispatcher answers itself** (no model spawn): **`/clear`** drops
   the chat's session and acks — the explicit "break the user↔session association"
   lever; **`/help`** and **`/start`** reply with the configured `help_text` (or a
   generic built-in), plain or as Telegram HTML (`help_html`). Telegram sends
   `/start` when a user first opens the bot, so
   intercepting it keeps that stray message off the responder. The `/clear` and
   `/help` menu is uploaded via `setMyCommands` at startup (best-effort).
2. Otherwise it looks up the chat's session, creates a **per-invocation outbox
   directory** (for the responder's document attachments and scratch), **mints a
   capability token** bound in memory to `Route{chat_id, reply_to=incoming
   message_id}` and that dir, and spawns the responder (`claude -p --agent …
   [--resume <id>]`) with `$AK_TGCLAUDE_OUTBOX` pointing at the dir, the token +
   MCP endpoint in its `--mcp-config`, and the message text on stdin. If the
   message carries a **document or a photo** (the largest rendition of a photo is
   taken), the dispatcher first fetches it (`getFile` + download) into an
   **`incoming/`** subdir of the outbox and names it in the prompt (with the
   caption as the instruction), so the responder can read or `Edit` it — an image
   included, since the Read tool renders it — and send it back with
   `send_document`; an attachment over `max_incoming_mb` (default 20 — the Bot API
   `getFile` ceiling) is refused with a note to the user instead. The
   responder delivers its replies by calling the dispatcher's MCP send tools,
   which resolve the route from the token and send **synchronously** (replying to
   the incoming one). For its lifetime the dispatcher shows a **`typing…`** chat
   action, refreshed every few seconds (Telegram expires it after ~5s) and stopped
   when the responder returns — so the user sees activity while the model thinks,
   and the gaps between a multi-message answer stay filled (each delivered message
   clears the action; the next refresh re-asserts it).
3. When the responder finishes, the session id it used (parsed from
   `--output-format json`) is bound to the chat, so the next message
   `--resume`s it. With **`bill`** (`--bill`) set, the round's `total_cost_usd`
   (also from that JSON) is sent to the chat as a bare **`$n.nnn`** message —
   only when it is present and non-zero, otherwise nothing. Under a Claude
   subscription the figure is *notional* (what the run would cost at API rates),
   not real billing. The **round** is the run plus any delivery-guard re-prompt
   (below): its cost is the two summed, and that same round figure feeds the
   usage log.

**Usage log (`usage_log` / `--usage-log`).** Point it at a file and the dispatcher
appends one compact **JSONL** line per answered round —
`{"ts","chat_id","user_id","msg_id","elapsed","cost"}` — for cost/latency
analytics; leave it empty (the default) and nothing is written. `ts` is the round's
start (RFC3339, host-local zone, whole seconds); `msg_id` is the incoming message's
Telegram id, so a row **joins the transcript store** (keyed on `chat_id` + `msg_id`)
for that turn; `elapsed` is the **whole-round** wall-clock in
seconds (a delivery-guard re-prompt is counted in, so it is *not* the per-`claude -p`
time); `cost` is the round-summed `total_cost_usd`, `0` when absent. The path is
the only switch, the parent dir is created if missing, and concurrent per-chat
rounds append safely. It is written by the dispatcher.

On the **read** side it is **owner-only**. When the feature is on, the owner's
responder is granted sandboxed read of the file (path injected into its prompt and
`$AK_TGCLAUDE_USAGE_LOG`, plus a **tg-usage** skill teaching it to aggregate the
JSONL and join names via the transcript store), so the owner can ask the bot cost
questions — "how much did we spend this week", "which chat costs the most". Every
**other** responder is *denied* read of the file (a per-invocation sandbox
`denyRead`; the file is otherwise readable by default, so this is what closes it).
The file appears in **no** static setting — each invocation carries exactly one of
allow (owner) / deny (everyone else), so allow-vs-deny precedence never arises. The
tg-usage skill is materialized but **not** preloaded into the agent (owner-only and
rare), so an ordinary user's turn never carries it.

**The per-invocation token is the route capability.** The dispatcher pins the
route in memory and binds it to the bearer token it handed this one responder; the
responder never names a chat, and the send tools take no `chat_id`, so it cannot
retarget a message — the route is decided by *which* token authorizes the call.
(See [The route capability is the token](#the-route-capability-is-the-token-not-a-directory).)

> The outbox dir no longer carries the route (the token does), but per-invocation
> **write isolation of that dir still matters**: a responder must write documents
> and scratch only to its **own** outbox, so a prompt-injected one cannot stage a
> file into a sibling's or read another's. That isolation is enforced by a
> `--settings` overlay on both the Write-tool and sandbox layers — see
> [Per-invocation isolation](#per-invocation-isolation-write-and-read).

Session ids are **not** derived from the chat id — Claude Code mints a fresh one
per new conversation and the dispatcher captures it, so `/clear` can truly sever
the association. The `chat→session` map and the poll offset are the dispatcher's
durable state (`$XDG_STATE_HOME/ak-tgclaude/sessions.json`).

Two operator levers beyond the per-chat `/clear` reset the whole map:

- **`ephemeral_sessions`** (`--ephemeral-sessions`) keeps the `chat→session` map
  **in memory only** — it is never written to disk, so every restart starts each
  chat fresh; any bindings still on disk from a previous run are **scrubbed at
  load** (not resurrected). The **poll offset still persists** (a restart doesn't
  reprocess the backlog); only the bindings are dropped. A standing "no
  cross-restart continuity" mode.
- The **`clear`** subcommand is the one-shot alternative: wipe the persisted
  bindings **now** (keeping the offset) without switching to ephemeral mode —
  `ak-tgclaude clear [--config bot.toml]` (state dir from the config or the
  default). Useful when the process is down, or to drop stale sessions whose
  on-disk transcripts or scaffold have since changed.

> Replying to an old bot message to **resurrect** its (since-cleared) session —
> a `message→session` map keyed by the sent `message_id` — is a planned follow-up
> (each send already returns its `message_id`).

### Smoke-testing the Telegram path (`--responder stub`)

`dispatch --responder stub` swaps the model for a stub that replies a fixed line
("i am here") to every message, delivered through the **real** MCP transport (an
actual `send_message` tools/call to the dispatcher's server) — so the full
Telegram I/O path runs (getUpdates → route → MCP `send_message` → `sendMessage`
with reply threading) without `claude` or a provisioned scaffold. It needs only a
token:

```sh
ak-tgclaude dispatch --responder stub --bot-token 123:ABC
```

Use it to verify connectivity, the bot token, long-polling, and reply routing
end to end before wiring the real responder.

## Transcripts & recall

`transcripts` (default **off**) turns on a durable, per-chat record of the
conversation that the responder can read back later — for context recovery ("I lost
context"), to resolve a message that **replies to** an older one, or for an owner's
"what did people ask this week" writeup. It is **off by default because it writes
users' message text to disk** (a privacy choice); the flag gates both the recording
and the `tg-recall` skill.

**What is recorded.** The dispatcher — the only component holding the trusted
`chat_id` and both sides of the conversation — writes every incoming user message
and every reply the model sends. Dispatcher-direct messages (help/clear acks,
errors) are not recorded. Text only: attachments are noted as metadata (kind, name,
size), never their bytes.

**On disk.** Under `transcript_dir` (default `<state>/transcripts`, which survives
restarts and the session-TTL outbox wipe — put an override **outside** the responder
outbox):

```
<root>/<chat_id>/
  meta.json            # who the chat is + first/last-seen + per-role counts;
                       #   private: username, first_name (the partner)
                       #   group:   type, title, username (the group itself)
  2026-07-04.jsonl     # one compact JSON record per turn, per day
```

Each record: `{"msg_id":N,"ts":"<RFC3339 local>","role":"user|bot","reply_to":N,"text":"…","attach":[…]}`,
plus optional fields — `part_of` (a split reply's later piece points at its anchor)
and, in a **group**, the author `user`/`name`/`username` (id / first name / @handle).
Day-files make "this week" the last seven files, and pruning a plain `find`:

```sh
find <root> -name '*.jsonl' -mtime +30 -delete   # admin retention, by hand
```

**Who can read what.** The responder reads the store through the **`recall` tool**
(`ak-tgclaude recall`, run as sandboxed Bash — see below), and the dispatcher
**scopes** each invocation from the trusted `from.id`, never from anything the model
controls:

- a normal user's responder is confined to that chat's own subdirectory;
- the **owner** (`owner`), when `owner_reads_all` (default true), reads the whole
  root — that is how cross-chat analytics works, in-band over Telegram. Set
  `owner_reads_all = false` to scope the owner like any user (e.g. when opening the
  bot to colleagues).

The scope is enforced at the read gates: the scaffold deny-reads the whole root and
the per-invocation settings `allowRead` only the scope, so the sandboxed `recall`
(and any raw `grep`) sees just that; the PreToolUse hook reads the same scope from
`AK_TGCLAUDE_TRANSCRIPT_DIR` for the `Read` tool. It is read-only.

**How the model uses it.** When the feature is on, a `tg-recall` skill is preloaded
into the responder (mirroring `tg-emit`) that drives the `recall` tool — a **point
lookup** (`--msg N [--context K]`, resolving a split reply to its anchor) or a
**range dump** (`--day`/`--since`/`--until [--role]`) returning groomed,
ready-to-read blocks instead of escaped JSONL. The binary path reaches the skill as
`$AK_TGCLAUDE_BIN` (set beside the scope), and the skill frames recalled text as
**untrusted reference**, not instruction. A replied-to message adds a one-line
prompt hint (`replies to msg N`); the model recalls the content by id only if it
needs it — nothing is auto-injected.

## Access control

The bot talks only to whitelisted users. `allowed_users` (config) / `--allow-user`
(repeatable) list the Telegram **user ids** allowed to use it, and the gate runs
before any command or the responder. Matching is by **numeric id** — usernames
are mutable and can be re-registered, so they are never a key (only a log label).

A user not on the list is **silently ignored**, except that `/start` and `/help`
get a single `no access for id N` line — that id is the user's own, so they can
report it to be whitelisted. This doubles as onboarding: a stranger's `/start`
hands them the id to relay; you add it and restart.

The list is **default-closed**: empty (and not open) denies everyone, so an
unconfigured bot is shut, not exposed. Bootstrap your own access by starting the
bot, sending `/start`, and whitelisting the id it reports (or pass
`--allow-user <id>` at launch). `open_access` / `--open` disables the gate
entirely — **demo only**, as it exposes the bot and the project it answers about
to anyone; it is logged loudly at startup.

The gate sits behind a one-method `Authorizer` seam, so a future stateful
allowlist (an admin `/allow <id>` mutating persisted state, composing with the
`no access for id N` id report) can replace the static list without touching the
routing.

## Groups

The bot works in group chats, not just one-on-one. Add it to a group and, in
**@BotFather → Bot Settings → Group Privacy, turn privacy OFF** — otherwise
Telegram only forwards commands and replies-to-the-bot, and the bot cannot see
(or record) the room's chatter. With privacy off it receives every message; how
it treats them:

- **Access is still per user id.** The whitelist gates the **sender**
  (`m.from.id`), never the chat. Being a member of a group the bot is in does
  **not** authorize you — admission ≠ authorization. An unauthorized member's
  messages get no reply at all (not even the `no access` line, which would leak
  the bot's presence and gate to the room); they are only recorded (below).
- **The bot answers only when addressed.** A group message spawns the responder
  only if the sender is authorized **and** the message is addressed to the bot —
  an **@mention** of its username, or the **`/do`** command. Everything else is
  chatter: recorded, not answered. So the bot listens to the room without
  reacting to every line. (With a single bot in the group, a bare `/do` is
  delivered and works; a second bot would need `/do@thisbot`.)
- **Chatter feeds recall.** Every group message (addressed or not, authorized or
  not) is appended to the [transcript](#transcripts--recall) when that feature is
  on, attributed to its author (`user` id, `name` = first_name, `username` =
  @handle). So when an authorized member
  later addresses the bot, its responder can recall what the room has been
  discussing. Group support is most useful with `transcripts` on; without it,
  unaddressed chatter is simply dropped.
- **`/do` delegates.** `/do <task>` runs the task as the commander's own request.
  `/do` sent as a **reply** to another message takes that message's content as the
  task — attributed to its (possibly unauthorized) author, but authorized by the
  member who typed `/do`, a human-in-the-loop endorsement. The delegated content
  is framed to the model as **untrusted input**, so a prompt-injection attempt
  inside a stranger's message can't redirect the bot (the read-only sandbox and
  the persona hold regardless). A reply whose target carried a file pulls that
  **file** too (see [transcripts](#transcripts--recall) — the bytes are re-fetched
  from Telegram, since the transcript keeps only metadata). The bot's answer
  threads under the original message.
- **Persona is a property of the group.** The injected persona is chosen by the
  **chat**, not by whoever speaks first — see [Policy](#policy-persona)
  (`group_policies` + a negative `policy_overrides` key). A configured owner
  speaking in a group gets the **group's** persona, not their private owner
  persona (they keep the latter in a one-on-one chat); they retain access
  everywhere via the user-id whitelist.
- **Session is shared per group.** One group is one conversation: all authorized
  members talk to the same session/context, keyed by the group's chat id.

The group `/do` (and `/clear`) menu is published to the `all_group_chats` scope
via `setMyCommands`, so clients autocomplete it in groups — this is cosmetic
only; the user-id gate, not the menu, decides what runs.

## Token isolation

The Telegram **bot token** is the asset to protect: whoever holds it controls the
bot. The responder is a Claude Code instance executing model-chosen tool calls on
untrusted input (arbitrary Telegram messages), so it must never be able to read
the token.

- The token is held **only in the dispatcher's memory**. It comes from one of three
  sources, in **descending order of safety**: **`bot_token_env`** (the dispatcher
  reads the named environment variable at startup and immediately `unset`s it —
  nothing on disk, and no exec'd child inherits it); **`--bot-token`** (a flag — on
  the host's `argv`, so invisible to the sandboxed responder but visible to other
  local processes via `ps`); or an inline **`bot_token`** in the config file (a
  secret **at rest on disk** — the weakest; see below). However it is sourced, the
  token is never exported to the responder's environment and never written to a path
  the responder can read.
- The responder reaches Telegram only **indirectly**, by calling the dispatcher's
  MCP send tools; the dispatcher is the only component that talks to the Telegram
  API. The per-invocation MCP token is a route capability, not the bot token: it
  only authorizes sending to the already-pinned chat, and Claude Code keeps it in
  the request header, out of the model's context.

**Sourcing the token — prefer `bot_token_env`.** When the token is inline in a
**config file**, the binary registers that file in the responder's
`sandbox.credentials.files` (`mode: "deny"`) so the sandbox denies any read of it — a
backstop to the PreToolUse hook (requires Claude Code ≥ 2.1.187). But that deny is a
**start-of-command snapshot** pinned to the file's inode, and the atomic `write-temp
+ rename` used to rewrite a secret slips past it
([window 2](#sandbox-masking-is-a-start-of-command-snapshot--two-leak-windows)
below) — so an inline token is a secret at rest behind a bypassable guard.
**`bot_token_env` avoids all of it:** point it at an environment variable holding the
token; the dispatcher reads it once at startup and `unset`s it before spawning
anything, so the token never touches disk and no child — above all the sandboxed
responder — can inherit it. `--bot-token` likewise keeps the token off disk (at the
cost of host `ps` visibility). Reserve inline `bot_token` for quick local runs, and
run [`ak-tgclaude audit`](#deployment-a-shared-host-with-live-secrets) to be warned
whenever a config still carries one.

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

### Host secrets beyond the bot token

The responder runs as the host user, so a prompt-injected `cat` in its sandboxed
shell could otherwise read the operator's own secrets. The generated
`settings.json` denies these unconditionally (independent of `--config`), each as a
**whole directory** via `sandbox.credentials.files` (`mode: "deny"`):

- **`~/.ssh`** — the user's SSH keys.
- **`~/.claude`** — Claude Code's entire home: its auth token
  (`.credentials.json`), the cross-session prompt/command history
  (`history.jsonl`), and the transcripts of the operator's other sessions
  (`projects/`, which may quote secrets from that work). Denying the **directory**
  (rather than enumerating those files) folds them into one entry and — crucially —
  survives the credentials file being **rewritten by rename** on token refresh,
  which a bare-file deny would not
  ([window 2](#sandbox-masking-is-a-start-of-command-snapshot--two-leak-windows)
  below).

This is the **Bash layer**; the **Read tool** is unsandboxed (only the hook gates
it), and since Read is default-open (it mirrors the sandbox) the hook carries these
same host secrets in its **absolute-deny** set — so neither Bash nor Read reaches
them. Denying `~/.claude` does **not** break the responder's own `claude -p` auth:
the parent process reads its credentials **unsandboxed**; only the Bash tools it
spawns are confined. (`~/` is expanded by the sandbox to the responder's home.)

**Extra paths — `deny_reads` / `--deny-read`.** To hide more paths (a secrets
file inside the project, a sibling repo, a mounted volume), list them in
`deny_reads` (or repeat `--deny-read <path>`; the two merge, like
`wire_skills`/`allowed_users`). Each path is denied at **both** layers: it joins the
hook's **absolute-deny** set (checked first, so it blocks the **Read tool** even for
a path that reads open by default) and `sandbox.filesystem.denyRead` blocks the
**sandboxed Bash**. Both are projected from the one config source — the dispatcher's
file policy for the hook, the generated settings for the sandbox. Paths take `~`
and, like every config path, resolve relative entries against the dispatcher's
**launch cwd** (see [Configuration](#configuration)).

**Extra env vars — `deny_envs` / `--deny-env`.** `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, and `CLAUDE_CODE_OAUTH_TOKEN` are **always** scrubbed from
the responder's sandboxed shell. To scrub more host secrets that leak through the environment, list their
**names** in `deny_envs` (or repeat `--deny-env <NAME>`; additive). They are
variable names, not paths — no `~`/relative resolution — and are added on top of
the defaults (never replacing them; duplicates are ignored). Each becomes a
`sandbox.credentials.envVars` deny entry.

**Extra egress domains — `allow_domains` / `--allow-domain`.** The responder's
sandbox permits egress only to the **Go-build defaults**
(`proxy.golang.org`/`sum.golang.org`/`storage.googleapis.com`). To let its
**sandboxed Bash** (a `curl`, a `go get` from another host) reach more hosts, list
them in `allow_domains` (or repeat `--allow-domain <domain>`; additive and
de-duplicated — the defaults are never dropped). They land in
`sandbox.network.allowedDomains`. A leading `*.` matches **subdomains only**, not
the apex (list the apex too if you need it). This is the **egress** layer, distinct
from a `WebFetch(domain:X)` grant in `tools`: that scopes the WebFetch **tool** and,
under `claude -p`, does **not** open sandbox egress — so a responder `curl`/`go get`
to a host needs *this* knob, not a WebFetch grant.

### Deployment: a shared host with live secrets

This bot is built to run **on a host where a person is also actively developing** —
the responder executes as that user, and the user's own secrets are **live and
changing while the bot runs**. That is the specific threat the masking below defends
against, and it is why the two leak windows are not academic here:

- **Credentials get rewritten under the running bot.** A `claude` token refresh, a
  `gh`/`aws` re-auth, an editor saving a dotfile — all land via the atomic
  `write-temp + rename` dance, which is exactly **window 2**: a file that *was*
  masked becomes readable the instant it is replaced.
- **New secrets appear mid-run.** The developer creates a `.env`, exports a key, or a
  build drops a credential into a directory the bot did not deny at startup — that is
  **window 1**: a path absent when the sandboxed command started is never masked.

So this deployment cannot rely on a one-time snapshot of *which secret files exist*
taken at launch. The hardening that follows — **deny whole directories, pre-created
before start, and source the responder's Claude auth from an env var rather than an
on-disk credentials file** — is chosen precisely because it survives a developer
mutating their secrets in parallel. The [`ak-tgclaude audit`](#sandbox-masking-is-a-start-of-command-snapshot--two-leak-windows)
subcommand (below) checks a given config against both windows.

### Sandbox masking is a start-of-command snapshot — two leak windows

The `credentials.files` and `filesystem.denyRead` masks above are installed by
bwrap **once, when each sandboxed command starts**, over the paths that exist at
that instant, and each mask is pinned to the target's **inode/dentry**, not to its
name. Because the sandbox is **per command** (every Bash tool call gets a fresh
namespace), a *short* command is indistinguishable from a durably-guarded one. A
**long-running** command (a build, a `go test`, a watch loop) opens two gaps —
both verified empirically:

1. **A secret absent at command start is never masked.** With nothing to bind
   over, bwrap skips the deny path and the parent directory stays a live bind of
   the host. A file created there *during* the command (a token written mid-run, a
   secret an earlier step generated) is then read in the clear by the
   already-running command.
2. **A rename over a masked path defeats the mask.** A path that *did* exist and
   *was* masked (reads blocked with `EACCES`) is still bypassed when the host
   replaces it via `rename(2)` — the standard atomic `write-temp + rename`. The
   mount stays pinned to the original, now-orphaned dentry, so a lookup of the name
   reaches the fresh inode unmasked. This is the sharper window: `write-temp +
   rename` is exactly how `~/.claude/.credentials.json` is rewritten on **token
   refresh**, so a refresh during a long sandboxed command would hand it the new
   token.

**Both windows are closed the same way: deny a directory, not individual files,
and make sure it exists before the responder starts.** A directory that exists at
namespace setup is masked as an **empty overlay** (a fresh tmpfs — `ino=1`, a
listing shows only `.`/`..`) that shadows the **whole subtree** for the life of the
command: every path inside reads `ENOENT`, whether it was present at start, created
later, or renamed in. (Verified: a running command with `~/.demo` denied never saw
a `secret.txt` that the host created and then renamed inside it.) Concretely:

- **Keep operator secrets in a dedicated directory and deny that directory** — via
  a `credentials.files` entry for a home credential store (this is exactly how the
  built-in `~/.ssh` is handled) or a `deny_reads` path for a project/sibling
  location — instead of enumerating individual files.
- **Pre-create the directory** before launching the bot. A deny on a
  not-yet-existing path is silently a no-op (window 1), so the guard only bites if
  the directory is already there for bwrap to overlay.
- **Prefer `CLAUDE_CODE_OAUTH_TOKEN` (in the dispatcher's environment) over an
  on-disk `~/.claude/.credentials.json`** for the responder's Claude auth. An env
  var is scrubbed fresh for *every* sandboxed command
  (`sandbox.credentials.envVars`, `mode: "deny"`) and so is immune to both
  filesystem windows, whereas the credentials file is rewritten by rename on
  refresh (window 2). `CLAUDE_CODE_OAUTH_TOKEN` is in the always-scrubbed default
  set beside the `ANTHROPIC_*` keys, so it never reaches the responder's shell; the
  parent `claude -p` still authenticates normally, reading the token before it
  spawns any sandboxed tool.

> A directory overlay is itself dentry-pinned, so renaming **the denied directory**
> and dropping a new one under the same name would re-open window 2 at the
> directory level — a far rarer event than rotating a file inside the directory,
> which the overlay fully covers.

**Check your setup — `ak-tgclaude audit`.** The `audit` subcommand classifies every
configured deny-secret by its on-disk shape and reports the window it is exposed to:
a path that **does not exist** (window 1), a **bare file** a rename can unmask
(window 2), or a **clean bill** when every secret is an existing directory. It also
flags a token stored literally in the config file, steering to `bot_token_env`. It
reads config exactly as the dispatcher does — the same TOML file **and** CLI flags,
overlaid (`flags > file`) — so `audit --config bot.toml [flags…]` reflects precisely
what that `dispatch` would mask, but it **never starts the bot**, so it is safe to
run against a live `bot.toml`. The dispatcher logs the same findings at startup, and
`scaffold` prints them in its inspection output.

```console
$ ak-tgclaude audit --config bot.toml
ak-tgclaude: auditing sandbox deny-secrets for mask-leak windows
  audited: /home/me/.ssh
  audited: /home/me/.claude
  audited: /home/me/.aws/credentials
  token source: config file /home/me/bot.toml (inline bot_token)
2 issue(s):
  - /home/me/.aws/credentials is a bare file: a file-level mask is bypassed when the file is replaced by rename … — keep the secret inside a whole-directory deny instead
  - the bot token is stored literally in /home/me/bot.toml: prefer bot_token_env …
```

## Responder (agent + emission skill)

The responder is a Claude Code **agent** launched per message. ak-tgclaude ships
a generic one, embedded in the binary and materialized into the scaffold:

- **`faq-responder`** (agent) — a read-only responder. The incoming message is its
  prompt; it explores the project at **`$AK_TGCLAUDE_PROJECT`** (set by the
  dispatcher) with Grep/Read/Bash and replies over Telegram. It is one persona-neutral
  **base** template (the invariant mechanics); its **persona** (see
  [Policy](#policy-persona) below) is composed per-user and injected at spawn, so one
  shared agent serves every chat. The default persona is a scoped FAQ that declines
  off-topic. It is the default `agent`. To give it **domain
  knowledge**, wire a skill into it with `wire_skills`/`--wire-skill` (see [Wiring
  domain skills](#wiring-domain-skills)) rather than writing a custom agent.
- **`tg-emit`** (skill, referenced by the agent's `skills:` frontmatter) — the
  **emission contract**: call the MCP send tools (`mcp__tg__send_message` /
  `send_code` / `send_document`), passing content directly as tool arguments — no
  files, no shell. Covers plain/HTML text, code blocks, document attachments, and
  multiple messages. The responder ends its turn with a **status word**
  (`answered` / `problematic` / `refused`) as its final message — not sent to
  Telegram; the dispatcher extracts it from the JSON output and logs it.

The dispatcher logs one line when it **launches** a responder (`chat` / `user` /
`msg`) and one when it **finishes** (adding `outcome` and duration), so each
`claude -p` is visible.

### Policy (persona)

The agent is **one persona-neutral base** — the invariant mechanics (project access,
replying via tg-emit, the machine-enforced boundaries). The **persona** is composed
per message and injected at spawn via `--append-system-prompt` (which composes with
`--agent` and freezes into the session), so one shared agent serves every chat and
the stance can vary **per user**. Set the default with `policies` (config) /
`--policy`, each selector one of:

- **`normal`** (default) — a read-only **FAQ assistant**, narrowly scoped: answers
  from the code, notes assumptions on ambiguity, declines off-topic, and treats the
  message as **untrusted** (won't follow instructions in it to change the rules).
- **`norefuse`** — a **do-what-you're-asked** assistant: acts on the message
  directly, doesn't decline as off-topic; when a tool call *is* denied it relays the
  concrete technical reason rather than a vague refusal.
- **`strict`** — a **hard-scoped** FAQ: answers only direct questions about the
  project from verifiable code, and declines anything else briefly, without
  elaborating or guessing.
- **`introspect`** — a candid **debug** persona: precise about *what* failed and
  *which* rule stopped it, explains how it reached an answer, and shares meta about
  its own context (which skills/agents are preloaded, what it read). (Distinct from
  the `--debug` flag, which is operator-side logging — see below.)
- **`outbox-rw`** — a **do-the-read-write-work** stance: when a task needs read-write
  on the project (e.g. does it still build after a `go get -u`, check out a commit and
  run its tests), don't beg off as read-only — clone into the writable outbox with
  `git clone --shared` and do it, sending a short progress note first. Axis-less, so it
  layers on top of any refusal stance (`strict + outbox-rw`, etc.). The machine
  boundaries still hold (writes land in the outbox, never the project).
- **a path to a `.md` file** — your own fragment, composed like a built-in (e.g.
  `--policy ~/lib/my-persona.md`).

`--policy help` prints the built-in catalog (each name with its one-line `summary:`)
and exits — handy under any subcommand (`ak-tgclaude dispatch --policy help`).

`policies` is a **list** — several selectors **merge in order** (blank-line
separated) into one persona, so stances layer (a built-in base plus a custom `.md`
overlay). In TOML it is an **array** (the plural-key convention; a bare string is also
accepted); `--policy` is repeatable and **additive** with the config list.

```toml
policies = ["norefuse", "~/lib/house-style.md"]   # or a bare string: policies = "normal"
```

**Axes.** A fragment may declare an `axis:` in its YAML frontmatter — an **opt-in**
mutual-exclusion guard. The refusal trio (`normal`/`norefuse`/`strict`) all carry
`axis: refusal`, so two of them in one persona is a **load-time error**; `introspect`,
`outbox-rw`, and axis-less custom fragments are purely additive. (The frontmatter is
stripped from the composed text; a fragment's `summary:` line, if any, feeds
`--policy help`.)

**Refusal-axis floor.** Because axis-less fragments are only *modifiers*, a persona
composed of nothing but them (e.g. a lone `--policy ./my-rw.md`, or `--policy
outbox-rw`) would have no base stance at all. So when the resolved list carries **no**
refusal-axis fragment, `normal` is prepended as the base — `--policy outbox-rw` becomes
`[normal, outbox-rw]`. To opt out (a deliberately base-less persona), give your custom
fragment its own `axis: refusal` and it takes the slot instead of `normal`.

**Per-user overrides.** `[policy_overrides]` maps a Telegram user id to a persona
layered on top of `policies` **along axes**: an override fragment that declares an
axis **evicts** the default fragment on that same axis, an axis-less one appends. So
with `policies = ["strict"]`, an override of `["norefuse"]` gives that user norefuse
(not both):

```toml
policies = ["strict"]

[policy_overrides]
12345678 = ["norefuse", "introspect"]   # this user: norefuse (evicts strict) + introspect
```

A **positive** `policy_overrides` key is a Telegram user id; the override applies in
that user's one-on-one chat, and the persona freezes into the session at its first
spawn.

**Group personas.** In a group the persona is a property of the **group**, not of
whoever speaks first. `group_policies` (config) / `--group-policy` is the default
group persona — layered on `policies` along axes exactly like a per-user override —
and a **negative** `policy_overrides` key is a specific group's chat id, layered on
that group base. User ids are positive and group chat ids negative, so both live in
one `[policy_overrides]` table without colliding:

```toml
policies       = ["strict"]     # private default
group_policies = ["norefuse"]   # default in any group (norefuse evicts strict on the refusal axis)

[policy_overrides]
12345678       = ["introspect"]   # a user (positive id): strict + introspect, in their DM
-1001234567890 = ["introspect"]   # one group (negative id): norefuse + introspect, for that group
```

Because the group persona is keyed by chat, it is deterministic regardless of who
speaks first (the session and persona freeze on the group's first spawn, but on the
group's persona, not the first speaker's).

**Owner shortcut.** `owner = <id>` (or `--owner`) names a Telegram user id that is
**auto-whitelisted** and granted the relaxed **owner persona** (`norefuse` +
`introspect`), unless it has an explicit `policy_overrides` entry. One knob for
"owner = admin" — the id must be supplied, since the Bot API's `getMe` does not
reveal the bot's owner. This is a positive (user) key, so it applies in the owner's
one-on-one chat; an owner speaking in a **group** gets the group persona, not the
owner persona (access is retained everywhere via the whitelist).

**Checking who got what.** Run with `--debug` to see the persona each account
resolves to: on a chat's **first spawn** the dispatcher logs the selector label (e.g.
`selectors=[normal]` vs `selectors=[norefuse introspect]`) and the exact composed
`--append-system-prompt` text. A non-owner with no `policy_overrides` entry gets the
default `policies` — so if a test account shows `norefuse`, that is your **default**
stance, not owner leakage. (Only on a fresh spawn: the persona freezes into the
session, so `clear` the chat to re-check after a config change.)

Every persona (or blend) is **safe by construction**: the read-only sandbox, token
deny-read, per-invocation write grant, and pinned route are machine-enforced, so no
persona can exceed them (it still can't modify the project, read secrets, or message
anyone but the sender). An unknown name, a missing fragment file, an axis conflict, or
a non-numeric override key is rejected **at startup**.

### Wiring domain skills

To turn the generic bot into a **domain expert**, you don't write a custom agent
— you **wire a skill** into the built-in `faq-responder`:

```toml
wire_skills = ["~/lib/eputs-qa-knowledge"]   # a skill DIRECTORY
```

(or `--wire-skill <path>`, repeatable and additive with the file list). Each entry
is a skill **directory** (its basename is the skill name): the whole tree is
copied, so bundled resources (reference material, scripts, a `selftest.sh`) come
along and their executable bits are preserved. A bare `SKILL.md` file is rejected
— copying only it would silently drop the skill's siblings. At startup each wired
skill is **materialized** into the scaffold's `.claude/skills/<name>/` and its name
is appended to `faq-responder`'s `skills:` frontmatter, so its **full body is
preloaded** into the responder's context. This is deliberate: a skill
merely present in `.claude/skills/` is loaded only **on-demand** (the model sees
its description and *may* invoke it), which is not reliable for a single-domain
bot; preloading via `skills:` guarantees it is always in context.

- **`{{PROJECT}}` substitution.** The responder's cwd is the disposable scaffold,
  not your project, so a skill can't use bare relative paths (`notes/…` would
  resolve under the scaffold). Write `{{PROJECT}}/notes/…` in the skill; it is
  replaced with the absolute `project` path at materialization, giving the
  Read/Grep tools (which don't shell-expand `$VARS` in path arguments) a literal
  path. A skill with no placeholder passes through unchanged, so ordinary
  project-agnostic skills wire in the same way.
- **Path resolution.** A wired path expands a leading `~`, and if relative
  resolves against the **dispatcher's launch cwd** (like `project`/`workdir`) — never
  against the project, since the template may live outside it. For a daemon
  (systemd, unpredictable launch cwd), use an absolute or `~` path.
- **Structural boundary.** `wire_skills` produces **only** skills under
  `.claude/skills/` — by construction it cannot introduce a `settings.json` (the
  token guard) into the scaffold.
- Wiring is read at **startup**, so changing a skill takes effect on the next
  dispatcher restart.

### Generic skills & agents (on-demand, not preloaded)

For **project-agnostic** helpers — a reusable skill or a specialist subagent that
isn't the bot's core domain — use `add_skills` / `add_agents` instead of `wire`:

```toml
add_skills = ["~/lib/md-export"]      # generic skill DIRECTORY(ies)
add_agents = ["~/lib/code-reviewer.md"]  # generic agent .md FILE(s)
```

(or `--add-skill <dir>` / `--add-agent <file.md>`, repeatable and additive.) The
difference from `wire_skills` is deliberate:

- **Copied verbatim** — no `{{PROJECT}}` substitution (these don't reference your
  project). A skill is a **directory** (whole tree copied, executable bits
  preserved, bundled resources come along); an agent is a single **`.md` file**.
- **Not preloaded** — they land in `.claude/skills/` and `.claude/agents/` for
  **on-demand** use. The responder sees their descriptions (the skill "table of
  contents") and pulls one in via the `Skill` tool / subagent delegation only when
  relevant. This is the right trade for a toolbox: `wire` guarantees a domain skill
  is *always* in context; `add` keeps generics out of context until needed.

Symlinking a shared canon instead of copying is **not** viable yet — Claude Code's
skill/agent discovery does not follow a symlinked directory (upstream bug), so
`add_*` copies. Since the scaffold is regenerated from source on every restart, an
edit to the canonical skill still propagates on the next restart.

## Responder scaffold (generated settings.json)

At startup the dispatcher **materializes** the responder's launch dir: it writes
a generated `.claude/settings.json`, copies the embedded agent + emission skill,
and launches `claude -p --setting-sources project --permission-mode dontAsk`
there, so only that file governs the responder (operator-global/local settings
are excluded, and an unmatched tool is denied rather than left hanging).

The settings are **built from a Go struct and marshaled to JSON** (not a text
template), so the literal runtime paths are inserted safely — `go:embed` is only
for static assets (the responder skills). Modeled on a real sandboxed tgbot
deployment, the generated file:

- enables the sandbox with `autoAllowBashIfSandboxed` (grep/`go`/`ak-tgclaude
  send` run without prompts) and `allowUnsandboxedCommands: false` (no escape);
- points the Go caches (`GOCACHE`/`GOMODCACHE`/…) at an **isolated** dir, and
  allows egress to `proxy.golang.org`/`sum.golang.org`/`storage.googleapis.com`
  (plus any operator `allow_domains`);
- grants sandbox writes to only the cache dir — **not** the outbox (that is
  per-invocation), and **deny-reads** the outbox area (see below);
- installs the **token guard**: `sandbox.credentials.files` deny-read on the
  config file, `credentials.envVars` deny (unset) for `ANTHROPIC_*`, and the
  PreToolUse hook `<self> hook pretooluse --deny-read <token file>`, where
  `<self>` is the dispatcher's **own absolute path** (`os.Executable()`, not the
  bare name): the security-critical hook must run the exact binary that wrote the
  settings, never whatever `ak-tgclaude` PATH resolves at hook-fire time.

Because the responder cwd is **write-denied** (`sandbox.filesystem.denyWrite`, so a
sandboxed Bash cannot rewrite the scaffold or `.claude`), the scaffold also
**pre-creates the mount-target stubs** Claude Code bind-mounts as read-only masks —
the empty root dotfiles (`.bashrc`, `.gitconfig`, `.mcp.json`, `.idea`, …), `.claude/commands`,
and the `.claude/.cc-writes` dir. A bind-mount needs its target to exist; on a
writable cwd bwrap creates a missing one, but under the deny that create hits
"read-only file system" and kills every sandboxed Bash — so they are laid down
while the scaffold is still writable. Types match the masks (files vs the one dir);
the list tracks Claude Code's internal mask set, and a future version masking a new
path fails a sandboxed Bash with EROFS naming it — the signal to add it.

The **PreToolUse hook** is the single authority for the **file tools**, driven by
one JSON policy the dispatcher hands it (`AK_TGCLAUDE_FILE_POLICY` — the same source
the sandbox settings project, so the two cannot drift):

- **Read** → **mirrors the sandbox**: default-OPEN, minus the masked roots (sibling
  outboxes, other chats' transcripts, a non-owner's usage log — the sandbox's
  `denyRead`), with this invocation's own scopes carved back (its outbox, its
  transcript scope, an owner's usage log — the sandbox's `allowRead`). So the Read
  tool reaches exactly what sandboxed Bash reaches — no project confinement, no
  "denied here, `cat` it via Bash" theatre that bought nothing (Bash could read it
  anyway). An **absolute-deny** set — host secrets (SSH keys, Claude
  credentials/history/projects), the token, operator `deny_reads` — is checked
  first and never carved, so the unsandboxed Read tool cannot slurp a secret even
  though reads are otherwise open.
- **Edit/Write/NotebookEdit** → a strict allowlist: this invocation's outbox
  (`$AK_TGCLAUDE_OUTBOX`, temp lands there too), else denied (the project and
  everything else stay non-writable; the responder authors only in its outbox).

Read default-open shifts weight onto the guard, so the hook `timeout` is set to an
effectively-infinite 24h: a PreToolUse hook that times out **fails open** (Claude
Code lets the tool proceed), which would let an attacker starve the machine to slip
a secret Read past a short window. The guard does microseconds of work, so it never
times out in practice; a crash still fails safe (a Go panic exits 2 = block, a parse
error self-denies).

It also **allows sandboxed** Bash / **denies unsandboxed** Bash, and **defers**
everything else (Grep/Glob/Skill/…) to the permission layer. With `bang_bug`
(`--bang-bug`, off by default) it additionally denies sandboxed Bash whose command
carries a `\!` — the signature of Claude Code bug #64301, where the sandbox
blind-escapes `!`→`\!` and silently corrupts the command/output; the model is told
to write the script to a file (heredoc included), which the sandbox runs verbatim.
A legitimate `\!` (e.g. `find … \!`) is caught too, hence opt-in. Because the hook
is the file-tool authority, the static `permissions.allow` lists only those
deferred tools (no Read/Write). A Bash read of the token is masked by the sandbox's
`credentials.files` deny-read, not the hook (obfuscation-proof; no command
string-matching). The hook reads the project/outbox from env, which the
dispatcher sets on the responder process and the (unsandboxed) hook inherits.

**Which `ak-tgclaude` runs.** The responder has exactly one `ak-tgclaude`
self-invocation site: the **PreToolUse hook**. It is emitted by the dispatcher, so
it is **pinned** to `os.Executable()` — PATH plays no part, and a stale or
shadowing `ak-tgclaude` cannot become the token guard. The responder no longer
runs an `ak-tgclaude send` subprocess (it emits via the MCP tools, which reach the
dispatcher's server over HTTP, not by re-exec), so there is no bare-name
self-invocation to keep in sync and no startup PATH check.

### Per-invocation isolation (write and read)

The responder writes document attachments and scratch files to its outbox. The
**Write tool** is scoped to this invocation's outbox (and the tmp dir) by the
**hook**, using `$AK_TGCLAUDE_OUTBOX`. The **sandbox** side (which governs Bash,
not the tools) is scoped by a per-invocation `--settings` overlay:

- `sandbox.filesystem.allowWrite: [<outbox>]` — so Bash (`cp`, a build that emits
  a file) can write only this outbox, not a sibling's. The outbox lives **outside**
  the responder cwd, so this is a **plain** grant on an otherwise-unwritable tree —
  never an `allowWrite` nested inside the project `denyWrite`, a precedence the docs
  do not promise (the read carve below is the documented direction; the write side
  is sidestepped by keeping the outbox a sibling);
- `sandbox.filesystem.allowRead: [<outbox>]` — carving this outbox back out of the
  static `denyRead: [<outboxRoot>]` so the responder can read files it authored
  while sibling outboxes stay masked.

These merge on top of the static settings (the `sandbox.filesystem` arrays merge
across `--settings` and the project file — verified empirically, including that a
flag `allowRead` overrides a file `denyRead` for the carved path). Only `hooks`
cannot be injected via `--settings`, which is why the hook lives in the
materialized file.

So a concurrent, possibly prompt-injected responder cannot **write** into another
chat's outbox (the hook denies a Write-tool path outside its own outbox; Bash
`cp` / redirect is denied by the sandbox), nor **read** another chat's staged
attachment (a Bash `cat`/`ls` of a sibling sees `No such file or directory`; a
Read tool outside the project is denied by the hook). This matters because
`send_document` attaches a file from the outbox: confining writes to the own
outbox, plus the server taking only a basename within *this* invocation's outbox,
closes the cross-chat confused-deputy on attachments — and the route itself is the
token's, never the responder's.

> Residual: the Grep/Glob **tools** are not path-scoped, so a determined, injected
> responder could in principle enumerate sibling outbox *names* (not Bash-maskable
> content) via those tools. Low value (random names; content reads are closed on
> both the Bash and Read-tool paths) — noted, not yet fenced.

### Static workdir vs ephemeral cwd, and `scaffold`

By default the responder cwd is **ephemeral** — a pseudo-random dir the
dispatcher removes on shutdown (SIGINT/SIGTERM). Set **`workdir`** (config or
`--workdir`) to use a **static** one instead: the responder cwd becomes
`$workdir/project`, regenerated from canon on every start (its contents are reset,
then the scaffold is re-materialized — so a removed wire-skill never lingers), and
the session store moves to `$workdir/state`. Because `$workdir/project` sits at a
stable path, the dispatcher marks it **trusted once** in `~/.claude.json` (trust is
keyed by path, so the per-start reset keeps it): a trusted workspace keeps its
`permissions.allow` and, on a vanilla build, its Grep/Glob tools. It is **not** a
hand-drop workspace — anything you leave in `project/` is wiped on the next start,
so add skills via `wire_skills`, not by hand. The Go build cache stays under
`state_dir`, shared across bots. (`workdir` is mutually exclusive with
`runtime_base`, which only governs the ephemeral cwd.)

The **`scaffold`** subcommand materializes `$workdir/project` **without** running
the dispatcher — for inspecting the sandbox in isolation (point it at a throwaway
`--workdir` so you don't disturb a live bot's project):

```sh
ak-tgclaude scaffold --workdir ~/qa-inspect --config bot.toml
# then run claude there by hand (the command is printed) to watch the sandbox
```

## Responder isolation

- Runs in its **own (ephemeral) cwd**, launched with `--setting-sources project`
  so only that project's generated `.claude/settings.json` is read —
  operator-global and local settings are excluded.
- Its writable surface is minimal: `sandbox.filesystem.allowWrite` grants only the
  **cache dir** (static) and this invocation's **outbox** (a per-invocation
  `--settings` overlay) — the responder cwd is otherwise read-only. Because settings
  paths are **not** environment-expanded, the binary writes the **literal** computed
  paths into the generated settings.json.
- Uses an **isolated module/tool cache** so its activity does not touch the host's.

## Outbound transport: an MCP server

The responder emits by **calling MCP tools** the dispatcher exposes, not by
dropping files in a spool. The dispatcher runs one long-lived **MCP-over-HTTP
server** on `127.0.0.1:<random port>/mcp`; each per-invocation responder is a
client, wired to it via `--mcp-config` (with `--strict-mcp-config` so no other MCP
source is picked up). The responder calls `send_message` / `send_code` /
`send_document`; the server delivers to Telegram **synchronously** and returns the
`message_id` (or a tool error).

Why MCP rather than the spool it replaces:

- **Native synchronous feedback.** The tool call returns the outcome — a
  `message_id` or the Telegram error — so a responder that emits bad HTML sees the
  failure in the same turn and fixes it, with no result-file polling.
- **Typed tool surface.** Content is passed as tool arguments (a JSON string), so
  text — quotes, `!`, arbitrary HTML — never touches a shell; the old
  write-to-a-file-and-`--file`-it dance is gone.
- **No upload-vs-teardown race for documents.** The call blocks until the upload
  finishes, so the file is guaranteed present (the spool's drain had to beat the
  per-invocation `RemoveAll`).

The trade-off, accepted deliberately: the transport is no longer **durable**. An
RPC is the "pipe without a reader" the spool avoided — if delivery fails, the call
returns an error then and there rather than queuing for a retry. For a FAQ bot
that is tolerable (the responder reports `problematic`), and the synchronous
feedback is worth more here than crash-safe queuing.

### The route capability is the token, not a directory

The Telegram **route** (`chat_id`/`reply_to`) is never chosen by the responder.
When the dispatcher spawns a responder for an update, it **mints a random bearer
token** and maps it in memory to that invocation's route (and its document
directory), then writes the token into the responder's `--mcp-config` as an
`Authorization: Bearer …` header. Claude Code attaches the header to every MCP
request under the hood, so **the token never enters the model's context**. The
server resolves the route from the token; the tool call carries no `chat_id`, so a
responder cannot retarget a message — the token *is* the route capability (as the
per-invocation outbox dir was in the spool design). The token is invalidated when
the responder exits.

This holds even under prompt injection: the token is not in the dialogue (so it
can't be exfiltrated), and even if it were, it only authorizes sending to the
already-pinned chat — which the responder can do anyway.

### The tools

The server (`tg`) exposes three tools; the route is pinned per invocation, so none
takes a `chat_id`. Rendering to Telegram HTML and the oversize-spill policy live in
the **dispatcher**, so a tool call only conveys intent.

- **`send_message(text, html?, silent?)`** — a text message. `html: true` sets
  Telegram `parse_mode=HTML` (the responder supplies valid, escaped HTML); default
  is plain, shown verbatim.
- **`send_code(code, language?, caption?, silent?)`** — a preformatted block,
  rendered as `<pre><code class="language-LANG">…</code></pre>` (the body escaped
  for you) and **spilled to a document** when it exceeds Telegram's size limit.
- **`send_document(path, filename?, caption?, silent?)`** — a file attachment. The
  responder writes the file into its **outbox** directory (`$AK_TGCLAUDE_OUTBOX`,
  its Write-tool scope) and passes the path.

A responder may call the tools several times to emit multiple messages for one
update (text, code, attachments, "think and send more").

**Large-file fallback (`upload_command`).** Telegram's bot API caps an attachment
near 50 MB. Set `upload_command` to an uploader script and a document over
`upload_threshold_mb` (default 40) is uploaded and delivered to the chat as a
**link** instead — transparently, so the responder still just calls
`send_document`. The dispatcher runs the command **unsandboxed** (it needs the
network), unlike the responder; the file stays confined to the outbox, and the
command is operator trust. Contract: invoked as argv `[command, <file>, <name>]` —
`<file>` is the local path and `<name>` is a **collision-free basename** (a random
prefix + the original name, e.g. `a3f9c2e1-dist.tar.gz`) a smart uploader uses as
its destination so two same-named files don't clobber each other on the share host
(an uploader that doesn't need it may ignore arg2, as long as it does not *reject* a
second argument); it prints the public URL on stdout (first non-blank line) and
exits 0, or exits non-zero with a message on stderr (surfaced to the model). The
name preserves the original filename, so it may contain non-ASCII (e.g. Cyrillic) —
a uploader that builds a URL should percent-encode it. `upload_max_mb` is the
ceiling advertised to the responder in the tg-emit skill; enforcement sits 10% above
it, so a file a touch over still uploads while a genuinely oversized one is rejected
with a clear error. Off by default (no `upload_command`) — big documents then just
hit Telegram's limit. See [`examples/`](examples/) for a ready rsync uploader.

Availability vs permission are two gates: the tools must appear in the responder
agent's `tools:` frontmatter (availability — an agent `tools:` allowlist filters
the toolset) **and** in `--allowedTools` (permission — under `--permission-mode
dontAsk` an unlisted tool is denied). Both gates are fed from **one** source: the
authored agent template carries no MCP tool names, only a `{{MCP_TOOLS}}` marker on
its `tools:` line, which the scaffold expands (at materialization) from the same
`mcpTools` list `dispatch` joins into `--allowedTools`. The MCP send tools are a
property of the invocation, not of the agent prose, so adding one touches only that
list — the marker's expansion carries its own leading separator (empty when there
are no MCP tools), so the `tools:` list stays comma-clean either way.

### Document path confinement

The server runs in the dispatcher (unsandboxed, full filesystem access), so it
**confines the document path itself** — the perimeter the sandboxed spool `send`
got for free from its read-scope. It takes only the **basename** of the path and
joins it to that invocation's outbox dir, so any directory component (including an
absolute path like `/home/…/.ssh/id_rsa`, or a `../` traversal) collapses to a
name that must exist in the outbox, else the call is rejected. The responder can
therefore attach only files it wrote to its own outbox.

### Delivery and errors

The tool handler builds an internal descriptor from the arguments, renders it
(`text` → `sendMessage` plain or `parse_mode=HTML`; `code` → `<pre><code>`;
oversize `text`/`code` → a spilled document; `document` → `sendDocument`), and
delivers it on the token's route. It returns the Telegram **`message_id`** on
success, or a **tool error** (`isError`) carrying the Telegram description — a
malformed-HTML `can't parse entities`, a blocked chat, an oversize attachment. The
responder acts on it: fix the HTML and call again (nothing was sent, so no
duplicate), or report `problematic`. A message over the 4096-char limit is not an
error — it spills to a document automatically.

Before an HTML message is sent, a guard validates it against Telegram's tag
whitelist and refuses it up front (nothing sent) if it carries an unsupported tag
(`<div>`, `<p>`, `<br>`, `<ul>`, `<li>`, `<hN>`, a stray Markdown-ism) — listing
**all** offenders at once, where Telegram's own 400 names only the first. So a
responder that emitted several bad tags fixes them in one round instead of
peeling them off one 400 at a time.

The `message_id` is also what the dispatcher will later map back to the
responder's session for **reply-resurrection** (replying to an old bot message
revives its `--resume` session).

## Install & deploy

The binary is distributed the normal Go way (`go install`), so by the time you run
it, it is already on `PATH`. The `deploy` subcommand therefore **does not copy
itself** — it provisions everything else (project root, example config, skills).
The responder no longer self-invokes `ak-tgclaude` (it emits via the MCP tools, not
a `send` subprocess), so nothing rides on the `PATH` name: the only self-reference
is the **PreToolUse hook**, and that is pinned to the dispatcher's absolute path
(`os.Executable()`) in the generated settings.json — see the **Which
`ak-tgclaude` runs** note above — so the token guard is unaffected by `PATH`.

## Approval UX

A motivating feature of the single-process design: the dispatcher can offer
inline-keyboard **yes/no approval buttons** in Telegram for gated actions.

## Repo layout

```
main.go            command dispatch (skeleton)
config.go          Config: TOML + CLI-flag resolution
mcp.go             the MCP-over-HTTP server: token->route registry, JSON-RPC, send_* tools, doc-path confinement
outbox.go          Descriptor: the in-memory outbound message model (built by the MCP handler)
deliver.go         sendDescriptor: render + spill + deliver on a route (the shared delivery core)
render.go          descriptor -> Telegram text/parse_mode, code wrapping, spill
telegram.go        Telegram Bot API client (getUpdates / sendMessage / sendDocument)
session.go         durable state: poll offset + chat->session map (+ ephemeral mode)
clear.go           `clear` subcommand: wipe persisted chat->session bindings
recall.go          `recall` subcommand: groomed transcript reader (point lookup / range dump)
responder.go       Responder interface (claude / stub) + `claude -p` spawn (MCP config wiring)
dispatch.go        the dispatch loop: poll -> route -> mint token -> respond (responder delivers via MCP)
scaffold.go        generated .claude/settings.json + materialize embedded assets (hook pinned to os.Executable())
assets/            embedded base agent + policy fragments + emission skill (go:embed)
hook.go            `hook pretooluse`: path-scope the file tools; deny protected reads (token + deny_reads)
deploy.go          `deploy`: example config + optional static workdir provisioning
bot.toml.example   example config
go.mod / go.sum
README.md          this design
deploy.sh          local dev build + install (gitignored, machine-specific)
```
