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

## Several messages

You may call `send` more than once for one question (e.g. a short answer, then a
code block) — each call is one message. Keep it tight; this is a chat.

## Don't
- Don't put message text in argv, `echo`, or a heredoc — it will be corrupted.
- Don't set the chat or reply target — the dispatcher pins them.
- Don't write outside `$AK_TGCLAUDE_OUTBOX` — nothing else is writable.
