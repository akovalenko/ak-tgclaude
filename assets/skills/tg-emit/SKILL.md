---
name: tg-emit
description: How an ak-tgclaude responder sends Telegram replies — call the MCP send tools (mcp__tg__send_message / send_code / send_document), passing the content directly as tool arguments. Covers plain/HTML text, code blocks, document attachments, length handling, and delivery errors.
---

# Sending Telegram replies (ak-tgclaude)

You are answering one Telegram message. You reply by calling the **MCP send
tools**. The dispatcher routes each message to the right chat and replies to the
incoming one — **you never choose the chat or the reply target.** The content
goes **directly in the tool call** — no files, no shell, no escaping quotes or
`!`.

## Plain vs HTML — decide, and match the flag to your intent

Every `send_message` is EITHER plain text OR Telegram HTML — you pick per message
with the `html` flag. Think about which your reply actually is, and don't mix them up.

**⚠ The one silent mistake:** if you write HTML tags (`<b>…`) but leave `html` off,
they go out as **literal text** — and Telegram does **not** return an error, so it
just quietly looks wrong (a bare `<b>` shown to the user instead of bold). The reverse
— plain text with a raw `<`/`&` sent as `html: true` — Telegram *does* reject, and you
fix and resend (see below). So the ONLY failure with no signal is HTML-without-the-flag.
Before you send, check the flag matches your intent.

**Plain** — `mcp__tg__send_message(text: "…")`, no `html`. Use it for prose with no
formatting, AND when you deliberately want the user to SEE literal markup — e.g.
answering a web question by quoting a tag: plain shows `<b>` verbatim, which is exactly
right. No escaping needed.

    mcp__tg__send_message(text: "Your answer here.")

**HTML** — `mcp__tg__send_message(text: "…", html: true)`. Use it whenever you want
formatting to render. Write **valid Telegram HTML**: escape `&`→`&amp;`, `<`→`&lt;`,
`>`→`&gt;` in the text, and the only allowed tags are
`<b> <i> <u> <s> <code> <pre> <a href="…"> <blockquote>`.

    mcp__tg__send_message(text: "<b>Bold</b> and <code>code</code>", html: true)

## Code / preformatted

`mcp__tg__send_code` with the raw `code` — it wraps in `<pre><code>` and escapes
for you, and spills to a document if it is too long. Do **not** hand-wrap code in
HTML.

    mcp__tg__send_code(code: "func main() {}", language: "go")

## Document / attachment

For a file you produced (e.g. a generated PDF): **write it into your outbox
directory first** with the Write tool (the outbox path is at the top of your
task — the only directory you can write to), then pass its path to
`mcp__tg__send_document`.

    mcp__tg__send_document(path: "<outbox>/report.pdf", caption: "summary")

Only files in your outbox can be attached; a path elsewhere is rejected.

## Authoring / scratch files

For non-trivial work — drafting a document, preparing an attachment — you can
**write, read, and edit** files in your **outbox** directory. A normal cycle is
Write a draft → Read it back → Edit it (the Edit tool requires you to have Read the
file first) → repeat, then attach the result with `mcp__tg__send_document`. Note
this is only for **attachments** — plain and code replies go straight through the
send tools with no file. The outbox is per-invocation and isolated. The project
directory is **read-only**.

## Several messages & length

You may call the send tools more than once for one question (e.g. a short answer,
then a code block) — each call is one message. Keep it tight; this is a chat.

Telegram caps one message at ~4096 characters. If a **text** answer would run
longer, split it yourself into several `send_message` calls at natural boundaries
(paragraphs or sections), each comfortably under ~4000 — a few readable messages
beat one wall of text. (An over-long message is still delivered, but as a *file
attachment* instead of inline text, which is worse to read — so split rather than
rely on that.) Long **code** needs no splitting: `send_code` spills an oversized
snippet to a document automatically, which is the right form for a big block.

## Progress notes for slow work (`progress: true`)

If answering needs slow work first — cloning the tree to scratch, running a build or
test, checking out a commit — send a brief **progress note** so the user is not left
in silence, and mark it `progress: true`:

    mcp__tg__send_message(text: "Building against go get -u in a scratch copy, one moment…", progress: true)

A `progress: true` message is delivered normally but does **not** count as your answer.
Send your actual answer as an ordinary send (no `progress`). So: narrate freely with
progress notes, then deliver the real reply — the "did you actually answer?" check keys
on real sends, not on progress notes.

## If a send fails

The send tool **returns an error** (a tool error) when the message did not get
through, with the reason. Read it, then split on what it says:

- **Invalid HTML — the one case you can fix.** An `html: true` body with an
  unescaped `<`/`&`, or a tag Telegram does not allow, comes back as a "can't
  parse entities" error. **Fix and resend**: escape the offending character, drop
  the disallowed tag, or fall back to plain `send_message` without `html`, then
  call the tool again. Nothing went out yet, so there is no duplicate.
- **Anything else** (blocked chat, chat-not-found, an attachment over Telegram's
  size limit, network trouble): you **cannot** fix the transport. Don't keep
  retrying — just note the reply did not get through and emit `problematic` as
  your final status word.

A message past Telegram's 4096-char limit is **not** an error: the dispatcher
automatically spills an over-long `send_message`/`send_code` to a document (you
still prefer to split long prose yourself — see above).

## Final output — output ONLY a status word (it is a signal, not your answer)

**Self-check before you finish:** did you actually call a send tool this turn? If
you did NOT, your reply has **not reached the user** — this final message is
discarded, never delivered. Call `mcp__tg__send_message` now (even just to say you
cannot help), THEN emit the status word. Do not put your answer in this final text.

Your answer has already gone to the user through a successful send tool call (if a
call reported an error, you fixed it and resent — see above). What you return now
— your final assistant message — is a **completion signal for the operator's log,
not data**. It must be **exactly one** of these words and **nothing else**:

- `answered` — you answered the question / did what was asked.
- `problematic` — you tried but could not fully complete it (blocked by policy,
  an error, or only a partial result).
- `refused` — you declined to do it.

**Hard rule:** do NOT repeat, summarize, confirm, or restate your answer in this
final output — no topic, no explanation, no extra words, no punctuation. Just the
one word. And it must be the **outcome category**, not a description of the action
you just took: `sent`, `done`, `ok`, `отправлено` are all **wrong** — the fact
that the reply went out is not the signal; whether you *answered*, hit a *problem*,
or *refused* is. The dispatcher does not show the word to anyone; any prose beyond
the token — or the wrong token — is noise the operator has to decode.

❌ `Подтверждаю — теперь работает. С дефолтным GOCACHE=… answered`
❌ `sent`  ← the action, not the outcome — say `answered`
✅ `answered`

The reply goes out via the send tools; then emit the single status word and stop.

## Don't
- Don't set the chat or reply target — the dispatcher pins them.
- Don't write outside your outbox directory — nothing else is writable.
- Don't send the status word to Telegram — it is your final assistant message, not a send call.
