package main

import (
	"html"
	"strings"

	"github.com/akovalenko/ak-tgclaude/internal/tghtml"
)

// telegramTextLimit is Telegram's per-message text cap, in UTF-16 code units. A
// rendered text/code message longer than this spills to a document instead.
const telegramTextLimit = 4096

// previewChunkBytes bounds the code carried by a single fenced block in a spilled
// .md. Telegram's mobile in-app document previewer truncates a fenced code block at
// ~8 KiB of content — measured empirically: two different line geometries (narrow
// 60-byte lines and wide 300-byte lines) both clipped at byte ~8180. The cap is per
// code-block, not per document (separate blocks each render in full), so oversized
// code is split across several fenced blocks rather than truncated. We chunk well
// under 8192 for headroom, and — since the cap's exact unit (raw bytes vs UTF-16
// code units) is unverified for multibyte source — count bytes, which is the safe
// side of either interpretation.
const previewChunkBytes = 7000

// renderMessage turns a text/code descriptor into the (text, parse_mode) pair
// for Telegram sendMessage. parseMode is "" for plain text, "HTML" otherwise.
// It returns ("","") for kinds it does not render inline (e.g. document).
func renderMessage(d *Descriptor) (text, parseMode string) {
	switch d.Kind {
	case KindText:
		if d.Format == FormatHTML {
			return d.Text, "HTML"
		}
		return d.Text, ""
	case KindCode:
		var b strings.Builder
		if d.Caption != "" {
			b.WriteString(html.EscapeString(d.Caption))
			b.WriteByte('\n')
		}
		b.WriteString("<pre>")
		if d.Language != "" {
			b.WriteString(`<code class="language-`)
			b.WriteString(html.EscapeString(d.Language))
			b.WriteString(`">`)
		} else {
			b.WriteString("<code>")
		}
		b.WriteString(html.EscapeString(d.Code))
		b.WriteString("</code></pre>")
		return b.String(), "HTML"
	}
	return "", ""
}

// fits reports whether s is within Telegram's per-message text limit. Telegram
// measures length in UTF-16 code units (an astral char — emoji, etc. — counts as
// two), so we count those, not runes: a rune count undercounts an emoji-heavy
// message and would let it through to a 400 instead of spilling. Counting the
// rendered string (markup included) is a safe overestimate — Telegram applies
// the cap to the text after entity parsing, and markup only adds units, so if
// the raw string fits, the parsed message fits too.
func fits(s string) bool {
	return tghtml.UTF16Len(s) <= telegramTextLimit
}

// spillName is the document filename for an oversized text/code message. Both
// spill as Markdown — Telegram renders a .md attachment in-app — so prose becomes
// message.md and code example.<lang>.md (example.go.md), or example.md when the
// snippet carries no language.
func spillName(d *Descriptor) string {
	if d.Kind == KindCode {
		if lang := spillLang(d.Language); lang != "" {
			return "example." + lang + ".md"
		}
		return "example.md"
	}
	return "message.md"
}

// spillLang reduces a code descriptor's language tag to a bare token safe to embed
// in the spill filename. It keeps only ASCII letters, digits, and the few marks
// that appear in real language names (+ # - _, as in c++, c#, objective-c),
// dropping path separators, dots, control characters, and any other junk, and
// caps the length so the filename stays bounded. An empty result (no language, or
// nothing survived) makes spillName fall back to example.md.
func spillLang(lang string) string {
	const maxLen = 32
	var b strings.Builder
	for _, r := range lang {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '#', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= maxLen {
			break
		}
	}
	return b.String()
}

// spillPayload is the Markdown body attached when a message spills to a document.
// Code becomes a fenced block (with its language); prose is converted from its
// Telegram-HTML rendering to Markdown. Plain text has no markup to convert and is
// attached as-is.
func spillPayload(d *Descriptor) string {
	if d.Kind == KindCode {
		return spillCodePayload(d.Language, d.Code)
	}
	if d.Format == FormatHTML {
		return tghtml.ToMarkdown(d.Text)
	}
	return d.Text
}

// spillCodePayload renders code as the Markdown body of a spilled .md. Code that
// fits a preview-sized block stays a single fenced block WITH its language, so the
// common small answer is syntax-highlighted as before. Larger code is split into
// consecutive fenced blocks joined by a blank line — and those blocks carry NO
// language: Telegram's mobile previewer highlights only the last few blocks of a
// multi-block document, so a language tag would color the tail unevenly (and would
// mangle a diff), while a bare fence renders every block uniformly plain. The blank
// line is what lets the previewer render each block in full: it resets the per-block
// ~8 KiB budget (see previewChunkBytes) and keeps the downloaded file clean with
// copy/paste across the seam intact. Splits fall on line boundaries, so concatenating
// the blocks' bodies reproduces the code verbatim.
func spillCodePayload(lang, code string) string {
	if len(code) <= previewChunkBytes {
		return tghtml.FencedCodeBlock(lang, code)
	}
	chunks := splitCodeForPreview(code, previewChunkBytes)
	blocks := make([]string, len(chunks))
	for i, c := range chunks {
		blocks[i] = tghtml.FencedCodeBlock("", c) // no language: uniform plain across blocks
	}
	return strings.Join(blocks, "\n\n")
}

// splitCodeForPreview splits code into consecutive chunks, each at most budget bytes
// and ending on a line boundary. It prefers to break just after a blank line, so a
// seam rarely lands inside a construct (a multi-line string, a function body) — a
// best-effort nicety, not a guarantee: a blank line inside a triple-quoted string can
// still be picked, and a run of non-blank lines a whole budget long falls back to a
// hard line-boundary break. Lines are kept verbatim, so the chunks concatenate back
// to the original. A single line longer than budget becomes its own over-budget chunk
// (it has no interior boundary to break at).
func splitCodeForPreview(code string, budget int) []string {
	lines := strings.SplitAfter(code, "\n") // each line keeps its trailing '\n'
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // SplitAfter yields a trailing "" when code ends in '\n'
	}
	var chunks []string
	start, size, lastBlank := 0, 0, -1
	for i := 0; i < len(lines); i++ {
		if i > start && size+len(lines[i]) > budget {
			end := i // hard break at this line boundary…
			if lastBlank > start {
				end = lastBlank // …unless a blank line offers a cleaner seam
			}
			chunks = append(chunks, strings.Join(lines[start:end], ""))
			start, size, lastBlank = end, 0, -1
			for _, l := range lines[end:i] {
				size += len(l) // carry lines already read past the break point
			}
		}
		size += len(lines[i])
		if strings.TrimSpace(lines[i]) == "" {
			lastBlank = i + 1 // a break here would sit just past the blank line
		}
	}
	if start < len(lines) {
		chunks = append(chunks, strings.Join(lines[start:], ""))
	}
	return chunks
}
