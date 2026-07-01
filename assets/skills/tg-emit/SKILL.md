---
name: tg-emit
description: How an ak-tgclaude responder sends Telegram replies — write the message body to a file in $AK_TGCLAUDE_OUTBOX and hand it to `ak-tgclaude send --file`, never putting message text on the command line. Covers plain/HTML text, code blocks, and document attachments.
---

# Sending Telegram replies (ak-tgclaude)

You are answering one Telegram message. You reply by dropping outbound messages
with the `ak-tgclaude send` command. The dispatcher routes each message to the
right chat and replies to the incoming one — **you never choose the chat or the
reply target.**

## The one rule: message text lives in a FILE, never on the command line

A sandboxed shell corrupts `!` and quotes, so message content must not appear as
a shell argument. For every message:

1. Write the body to a file **in your outbox directory** with the **Write tool**.
   The outbox path is given at the top of your task (the "Outbox directory"
   line) — it is the only directory you can write to. Use that **literal path**
   in the Write tool: the tool does NOT expand `$AK_TGCLAUDE_OUTBOX` in its
   `file_path` (only the shell does), so writing to `"$AK_TGCLAUDE_OUTBOX/reply.txt"`
   as a tool path would create a file named literally that.
2. Hand the file to `send --file`. This is a **shell** command, so here
   `$AK_TGCLAUDE_OUTBOX` DOES expand — either form works:
   `--file "$AK_TGCLAUDE_OUTBOX/reply.txt"` or the literal path from step 1.
   The command line carries only flags and a filename — no message text.

Never build the message with `echo`/`printf`/`cat <<EOF` or pass it as a `send`
positional argument.

## Plain text (default, safest)

    ak-tgclaude send text --file "$AK_TGCLAUDE_OUTBOX/reply.txt"

No escaping needed. Prefer this for ordinary answers.

## Rich text (Telegram HTML)

Add `--html` and write **valid Telegram HTML**: escape `&`→`&amp;`, `<`→`&lt;`,
`>`→`&gt;` in text; the only allowed tags are
`<b> <i> <u> <s> <code> <pre> <a href="…"> <blockquote>`.

    ak-tgclaude send text --file "$AK_TGCLAUDE_OUTBOX/reply.html" --html

## Code / preformatted

Do **not** hand-wrap code in HTML. Use `send code` with the raw code — it wraps
in `<pre><code>` and escapes for you, and spills to a document if it is too long:

    ak-tgclaude send code --file "$AK_TGCLAUDE_OUTBOX/snippet.go" --lang go

## Document / attachment

For a file you produced (e.g. a generated PDF), send it as a document:

    ak-tgclaude send doc "$AK_TGCLAUDE_OUTBOX/report.pdf" --caption "summary"

## Authoring / scratch files

For non-trivial work — drafting a document, iterating on a long answer, preparing
an attachment — you can **write, read, and edit** files in your writable areas:
your **outbox** directory or the sandbox tmp (`/tmp/claude-<uid>`). A normal cycle
is Write a draft → Read it back → Edit it (the Edit tool requires you to have Read
the file first) → repeat, then send the result (`send --file` or `send doc`).

Prefer the **outbox** for anything private: it is per-invocation and isolated, so
no other responder can see it. The tmp dir is **shared** across concurrent
responders — fine for throwaway scratch, but don't leave anything there you
wouldn't want a concurrent invocation to read. The project directory is
**read-only** (you can read it, not write it).

## Several messages & length

You may call `send` more than once for one question (e.g. a short answer, then a
code block) — each call is one message. Keep it tight; this is a chat.

Telegram caps one message at ~4096 characters. If a **text** answer would run
longer, split it yourself into several `send text` calls at natural boundaries
(paragraphs or sections), each comfortably under ~4000 — a few readable messages
beat one wall of text. (An over-long message is still delivered, but as a *file
attachment* instead of inline text, which is worse to read — so split rather than
rely on that.) Long **code** needs no splitting: `send code` spills an oversized
snippet to a document automatically, which is the right form for a big block.

## If `send` fails

`send` waits for delivery and **exits non-zero if the message did not get
through**, printing the reason to stderr. Read that stderr line, then split on
what it says:

- **Invalid HTML — the one case you can fix.** A `--html` body with an unescaped
  `<`/`&`, or a tag Telegram does not allow, comes back as a "can't parse
  entities" error. **Fix and resend**: escape the offending character, drop the
  disallowed tag, or fall back to plain `send text` without `--html`, then run
  `send` again. Nothing went out yet, so there is no duplicate.
- **Anything else** (blocked chat, chat-not-found, an attachment over Telegram's
  size limit, network trouble that exhausted retries): you **cannot** fix the
  transport. Don't keep retrying — just note the reply did not get through and
  emit `problematic` as your final status word.

Two things that are **not** failures — don't treat them as such:

- A message past Telegram's 4096-char limit is **not** rejected: the dispatcher
  automatically spills an over-long `text`/`code` to a document (you still
  prefer to split long prose yourself — see "Several messages & length").
- A `send` that prints `queued; delivery outcome unknown after 5s` and exits `0`
  is fine — the dispatcher was just slow to confirm; the message is queued and
  will go out.

## Final output — output ONLY a status word (it is a signal, not your answer)

Your answer has already gone to the user through a successful `ak-tgclaude send`
(if a `send` reported an error, you fixed it and resent — see above). What you
return now — your final assistant message — is a **completion signal for the
operator's log, not data**. It must be **exactly one** of these words and **nothing else**:

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

The reply goes out via `ak-tgclaude send`; then emit the single status word and
stop.

## Don't
- Don't put message text in argv, `echo`, or a heredoc — it will be corrupted.
- Don't set the chat or reply target — the dispatcher pins them.
- Don't write outside `$AK_TGCLAUDE_OUTBOX` — nothing else is writable.
- Don't send the status word to Telegram — it is stdout only.
