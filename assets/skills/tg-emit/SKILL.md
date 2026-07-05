---
name: tg-emit
description: How an ak-tgclaude responder sends Telegram replies — call the MCP send tools (mcp__tg__send_message / send_code / send_document), passing the content directly as tool arguments. Covers plain/HTML text, code blocks, document attachments, length handling, and delivery errors.
---

# Sending Telegram replies (ak-tgclaude)

You are answering one Telegram message. You reply by calling the **MCP send
tools**. The dispatcher routes each message to the right chat and replies to the
incoming one — **you never choose the chat or the reply target.** The body is
passed to the send tool — inline for text you write yourself, or as a file in your
outbox for content another tool produced (see *Tool-generated content* below) —
with no shell and no quote/`!` escaping either way.

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

## Tool-generated content — send it from a file, verbatim

When the body you send was **produced by another tool** — a script that emits
Telegram HTML (`make-tg-html.py > reply.tg.html`), a generator that prints plain
text, `gitlab-links` output carrying commit SHAs — do **not** retype or paraphrase
it into your reply. Write it to a file in your outbox and send **that file**, so it
reaches Telegram exactly as produced:

    mcp__tg__send_message(text_file: "reply.tg.html", html: true)   # HTML from a tool
    mcp__tg__send_message(text_file: "reply.txt")                   # plain text from a tool
    mcp__tg__send_code(code_file: "snippet.go", language: "go")     # a file echoed verbatim

Pass the **basename** of a file in your outbox, and supply exactly one of the inline
arg (`text`/`code`) or the file arg (`text_file`/`code_file`). You may read the file
first to sanity-check it, but then send the **file itself** — never a hand-copied
version, where a stray edit could corrupt an SHA, an escape, or a tag. Retype only
content you are authoring yourself.

## Document / attachment

For a file you produced (e.g. a generated PDF): **write it into your outbox
directory first** with the Write tool (the outbox path is at the top of your
task — the only directory you can write to), then pass its path to
`mcp__tg__send_document`.

    mcp__tg__send_document(path: "<outbox>/report.pdf", caption: "summary")

Only files in your outbox can be attached; a path elsewhere is rejected.

## Code block or file — match the form to what they'll do with it

Between an inline code block (`send_code`) and an attachment (`send_document`), read
what the user means to **do** with the content — the form follows the intent, not
the size:

- **To read / look at it** — "show me the diff", "покажи дифф", "what does that
  function look like" — send an **inline code block** with `send_code`. It renders
  in the chat, so they can just glance at it.
- **To use it as a file** — "send me main.go", "пришли патч", "give me the file so I
  can apply it" — send an **attachment** with `send_document`. They save it or run
  `git am` on it; a patch pasted into a code block cannot be applied.

The trap is a **patch or a whole file**: asked for something to apply, sent as a code
block, it *looks* right but `git am` has nothing to act on. When you are unsure
between "to look at" and "to keep", a file is the safer choice for anything they
might feed to a tool. (`send_code` can take its body from a file too — see
*Tool-generated content* above — but that only controls where the text comes from;
this choice, block vs attachment, is about what the user does with it.)

{{UPLOAD_NOTE}}## Authoring / scratch files

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

About **4000 characters** is the comfortable size for one Telegram message. A
message can hold more (the hard ceiling is higher — around **16k and up, depending
on the chat's settings**), but writing to that edge is a wall of text. So keep each
reply **compact** and to the point — that is the whole of the length guidance. You
do **not** manage the size limit yourself and you do **not** pre-chop a reply to
fit it: an over-long answer is handled for you (delivered as a Markdown
**document**), and long **code** the same way — `send_code` spills a big block to a
document, the right form for it. If a question genuinely has separate parts, a
couple of short messages is fine — but that is for readability, never a way to
squeeze under a size cap.

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
- **Message too long — shorten or attach, never chunk.** A send can come back with
  a too-long / size error (a deployment can be set to hand an oversized message back
  to you rather than spill it for you). Do **one** of two things: tighten the reply —
  cut or restructure it so it fits — or, when the content must stay whole (a long
  listing, a full file), write it into a `.md` file in your outbox and send that with
  `send_document`. Do **not** slice the reply into a series of messages to get under
  the cap. Chunking defeats the limit — the point of a size limit is lost the moment
  the model works around it. Shorten, or attach as a document; never chunk.
- **Anything else** (blocked chat, chat-not-found, an attachment over Telegram's
  size limit, network trouble): you **cannot** fix the transport. Don't keep
  retrying — just note the reply did not get through and emit `problematic` as
  your final status word.

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
