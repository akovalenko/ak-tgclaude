# ak-tgclaude

A single-user **Telegram FAQ bot built on Claude Code**. One Go binary acts as a
long-lived **dispatcher** that receives Telegram updates and routes each one to a
**project-bound responder** — a headless `claude -p` session that answers from a
codebase and its notes — then sends the reply back to Telegram.

> Status: **implemented, under active development**. The dispatcher, the
> project-bound responder (`claude -p`, plus a `stub` for Telegram-I/O smoke
> tests), the MCP-over-HTTP outbound transport with synchronous delivery feedback,
> and the supporting subcommands (`scaffold` / `clear` / `deploy` / `hook`) are
> built and unit-tested. This README remains the design of record; the non-QA
> profiles are reserved but not wired yet.

## Why one binary (multitool)

Everything is a single Go binary, selected by its first argument — no shell
sprawl, one thing to put on `PATH`:

| mode | where it runs | what it does |
|------|---------------|--------------|
| `dispatch` | host (trusted) | holds the bot token in memory, polls Telegram `getUpdates`, routes each update to a responder, and runs the MCP server that delivers the responder's replies to Telegram |
| `hook pretooluse` | as the responder's PreToolUse hook | gates the responder's tool calls (e.g. denies reads of the token file) |
| `scaffold` | host | materializes a responder `workdir/project` (generated settings.json) without running the dispatcher, to inspect it and run `claude` by hand |
| `clear` | host | drops every persisted chat→session binding (keeps the getUpdates offset); reads the state dir from `--config` or the default |
| `deploy` | host, once | writes an example config and (with `--workdir`) provisions the static workdir + marks it trusted |

## Configuration

Configuration comes from a **TOML file**, **CLI flags**, or both — flags override
the file, the file overrides defaults (`flags > file > defaults`). A minimal
config (`bot.toml`, see `bot.toml.example`):

```toml
bot_token = "123456789:AA..."   # secret; kept in dispatcher memory, never in env
profile   = "qa"                # qa (read-only, default) | dev | ops (reserved)
project   = "~/code/myproject"  # the codebase consulted on (read-only under qa)
# wire_skills = ["~/lib/eputs-qa-knowledge"]  # domain skill(s) preloaded into the responder
# deny_reads = ["~/code/myproject/secrets.env"]  # extra paths the responder must never read
# deny_envs  = ["MY_SECRET"]     # extra env-var names to scrub (ANTHROPIC keys are always scrubbed)
# allow_domains = ["api.github.com"]  # extra sandbox egress domains, on top of the Go-build defaults
# claude_args = ["--model", "opus", "--effort", "high"]  # extra raw `claude -p` flags (ak-tgclaude-owned flags rejected)
# allow_silent = false          # true DISABLES the delivery guard (below); default false = guard on
# undelivered_text = "Sorry, I could not answer that."  # fallback reply if the guard's re-prompt still sent nothing
# upload_command = "~/uploaders/rsync-upload.sh"  # large-file fallback: docs over ~40 MB uploaded and sent as a link (below; see examples/)
# tools = ["Agent", "WebFetch(domain:*.github.com)"]  # grant EXTRA tools: bare name→frontmatter, full spec→--allowedTools; sharp knob — see below
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
- **Paths.** Every path field (`project`, `workdir`, `wire_skills`, `add_skills`,
  `add_agents`, `deny_reads`, `state_dir`, `runtime_base`, `--config`) takes a
  leading `~` and is made
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
   MCP endpoint in its `--mcp-config`, and the message text on stdin. The
   responder delivers its replies by calling the dispatcher's MCP send tools,
   which resolve the route from the token and send **synchronously** (replying to
   the incoming one). For its lifetime the dispatcher shows a **`typing…`** chat
   action, refreshed every few seconds (Telegram expires it after ~5s) and stopped
   when the responder returns — so the user sees activity while the model thinks,
   and the gaps between a multi-message answer stay filled (each delivered message
   clears the action; the next refresh re-asserts it).
3. When the responder finishes, the session id it used (parsed from
   `--output-format json`) is bound to the chat, so the next message
   `--resume`s it. With **`bill`** (`--bill`) set, the run's `total_cost_usd`
   (also from that JSON) is sent to the chat as a bare **`$n.nnn`** message —
   only when it is present and non-zero, otherwise nothing. Under a Claude
   subscription the figure is *notional* (what the run would cost at API rates),
   not real billing.

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

## Token isolation

The Telegram **bot token** is the asset to protect: whoever holds it controls the
bot. The responder is a Claude Code instance executing model-chosen tool calls on
untrusted input (arbitrary Telegram messages), so it must never be able to read
the token.

- The token is held **only in the dispatcher's memory** (parsed from the TOML
  config, or read from `--bot-token` at startup). It is never placed in an
  environment variable and never written to a path the responder can read.
- The responder reaches Telegram only **indirectly**, by calling the dispatcher's
  MCP send tools; the dispatcher is the only component that talks to the Telegram
  API. The per-invocation MCP token is a route capability, not the bot token: it
  only authorizes sending to the already-pinned chat, and Claude Code keeps it in
  the request header, out of the model's context.
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

### Host secrets beyond the bot token

The responder runs as the host user, so a prompt-injected `cat` in its sandboxed
shell could otherwise read the operator's own secrets. The generated
`settings.json` denies these unconditionally (independent of `--config`):

- **`~/.ssh`** and **`~/.claude/.credentials.json`** (the user's SSH keys and
  Claude Code's own auth token) via `sandbox.credentials.files` (`mode: "deny"`);
- **`~/.claude/history.jsonl`** (cross-session prompt/command history) and
  **`~/.claude/projects`** (transcripts of the operator's other sessions, which
  may quote secrets from that work) via `sandbox.filesystem.denyRead`.

This is the **Bash layer** only — the file tools (`Read`/`Grep`/`Glob`) are
already confined to the project by the PreToolUse hook (default-closed allowlist),
so they can't reach these paths either. Denying `~/.claude/.credentials.json` does
**not** break the responder's own `claude -p` auth: the parent process reads its
credentials **unsandboxed**; only the Bash tools it spawns are confined. (`~/` is
expanded by the sandbox to the responder's home.)

**Extra paths — `deny_reads` / `--deny-read`.** To hide more paths (a secrets
file inside the project, a sibling repo, a mounted volume), list them in
`deny_reads` (or repeat `--deny-read <path>`; the two merge, like
`wire_skills`/`allowed_users`). Each path is denied at **both** layers: the hook
blocks the **Read tool** (checked before the project-read allow, so it wins even
for a path *inside* the project) and `sandbox.filesystem.denyRead` blocks the
**sandboxed Bash**. Paths take `~` and, like every config path, resolve relative
entries against the dispatcher's **launch cwd** (see [Configuration](#configuration)).

**Extra env vars — `deny_envs` / `--deny-env`.** `ANTHROPIC_API_KEY` and
`ANTHROPIC_AUTH_TOKEN` are **always** scrubbed from the responder's sandboxed
shell. To scrub more host secrets that leak through the environment, list their
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

## Responder (agent + emission skill)

The responder is a Claude Code **agent** launched per message. ak-tgclaude ships
a generic one, embedded in the binary and materialized into the scaffold:

- **`faq-responder`** (agent) — a read-only responder. The incoming message is its
  prompt; it explores the project at **`$AK_TGCLAUDE_PROJECT`** (set by the
  dispatcher) with Grep/Read/Bash and replies over Telegram. It is composed from one
  **base** template (the invariant mechanics) plus a swappable **policy** fragment
  (its persona — see [Policy](#policy-persona) below); the default persona is a
  scoped FAQ that declines off-topic. It is the default `agent`. To give it **domain
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

The agent is **one base template** — the invariant mechanics (project access,
replying via tg-emit, the machine-enforced boundaries) — with a `{{POLICY}}` marker
that the scaffold replaces with a **persona fragment**. The persona is a property of
the invocation, so there is exactly one copy of the shared prose; swapping the stance
never touches it. Select with `policy` (config) / `--policy`, each of which is one of:

- **`normal`** (default) — a read-only **FAQ assistant**, narrowly scoped: answers
  from the code, notes assumptions on ambiguity, declines off-topic, and treats the
  message as **untrusted** (won't follow instructions in it to change the rules).
- **`norefuse`** — a **do-what-you're-asked** assistant: acts on the message
  directly, doesn't decline as off-topic; when a tool call *is* denied it relays the
  concrete technical reason rather than a vague refusal.
- **`introspect`** — a candid **debug** persona: precise about *what* failed and
  *which* rule stopped it, explains how it reached an answer, and shares meta about
  its own context (which skills/agents are preloaded, what it read). (Distinct from
  `--debug`, which toggles claude's own transport diagnostics.)
- **a path to a `.md` file** — your own fragment, composed into the base like a
  built-in (e.g. `--policy ~/lib/my-persona.md`).

`policy` is **repeatable** — give several selectors and they are **merged in order**
(blank-line separated) into one persona, so stances layer (e.g. a built-in base plus a
custom `.md` overlay). In TOML it accepts a bare string or an array; `--policy` is
repeatable and **additive** with the config list (like `wire_skills` et al.):

```toml
policy = ["norefuse", "~/lib/house-style.md"]   # or a bare string: policy = "normal"
```

```
--policy norefuse --policy ~/lib/house-style.md   # merged on top of any config entries
```

Every persona (or blend) is **safe by construction**: the read-only sandbox, token
deny-read, per-invocation write grant, and pinned route are machine-enforced, so no
persona can exceed them (it still can't modify the project, read secrets, or message
anyone but the sender). An unknown name or a missing fragment file is rejected **at
startup**.

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

The **PreToolUse hook** is the single authority for the **file tools**,
path-scoped from the responder's env:

- **Read** → allowed under the project (`$AK_TGCLAUDE_PROJECT`) **and the writable
  areas** (so the responder can read back what it authored), else denied (read
  elsewhere with sandboxed Bash);
- **Edit/Write/NotebookEdit** → allowed under this invocation's outbox
  (`$AK_TGCLAUDE_OUTBOX`) or the sandbox tmp (`/tmp/claude-<uid>`), else denied
  (so the project stays read-only but the responder can author/iterate on files);
- a **protected-path** touch (`--deny-read`: the token file, plus any operator
  `deny_reads`/`--deny-read`) is denied first — it wins even if that path happens to
  sit under the project.

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
  a file) can write only this outbox, not a sibling's;
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
