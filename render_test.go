package main

import (
	"strings"
	"testing"
)

func TestRenderText(t *testing.T) {
	if got, mode := renderMessage(&Descriptor{Kind: KindText, Text: "hi"}); got != "hi" || mode != "" {
		t.Errorf("plain: got %q mode %q", got, mode)
	}
	if got, mode := renderMessage(&Descriptor{Kind: KindText, Text: "<b>x</b>", Format: FormatHTML}); got != "<b>x</b>" || mode != "HTML" {
		t.Errorf("html: got %q mode %q", got, mode)
	}
}

func TestRenderCode(t *testing.T) {
	got, mode := renderMessage(&Descriptor{Kind: KindCode, Code: "a < b && c", Language: "go", Caption: "f<>"})
	if mode != "HTML" {
		t.Fatalf("mode = %q, want HTML", mode)
	}
	if !strings.Contains(got, `<pre><code class="language-go">`) {
		t.Errorf("missing language-tagged pre/code: %q", got)
	}
	if !strings.Contains(got, "a &lt; b &amp;&amp; c") {
		t.Errorf("code body not HTML-escaped: %q", got)
	}
	if !strings.HasPrefix(got, "f&lt;&gt;\n") {
		t.Errorf("caption not escaped/prefixed: %q", got)
	}
	if !strings.HasSuffix(got, "</code></pre>") {
		t.Errorf("not closed: %q", got)
	}

	// No language => bare <code>.
	bare, _ := renderMessage(&Descriptor{Kind: KindCode, Code: "x"})
	if !strings.Contains(bare, "<pre><code>") {
		t.Errorf("bare code block wrong: %q", bare)
	}
}

func TestFitsAndSpill(t *testing.T) {
	if !fits("short") {
		t.Errorf("short text should fit")
	}
	big := strings.Repeat("x", telegramTextLimit+1)
	if fits(big) {
		t.Errorf("oversized text should not fit")
	}

	// Astral chars count as two UTF-16 units each: half-the-limit-plus-one emoji
	// is under the rune limit but over the UTF-16 limit — must NOT fit (a rune
	// count would wrongly pass it and let Telegram reject it).
	astral := strings.Repeat("😀", telegramTextLimit/2+1)
	if fits(astral) {
		t.Errorf("emoji string over the UTF-16 limit should not fit (runes=%d, utf16=%d)",
			len([]rune(astral)), utf16Len(astral))
	}
	// Exactly at the limit fits.
	if !fits(strings.Repeat("x", telegramTextLimit)) {
		t.Errorf("text exactly at the limit should fit")
	}

	// Both kinds spill as Markdown: code as example.<lang>.md, prose as message.md.
	if n := spillName(&Descriptor{Kind: KindCode, Language: "py"}); n != "example.py.md" {
		t.Errorf("spillName code = %q", n)
	}
	// No language => example.md (no dangling dot).
	if n := spillName(&Descriptor{Kind: KindCode}); n != "example.md" {
		t.Errorf("spillName code, no lang = %q", n)
	}
	// A junk-laden language is sanitized to a bare token before it lands in the name.
	if n := spillName(&Descriptor{Kind: KindCode, Language: "../etc/pas swd\n"}); n != "example.etcpasswd.md" {
		t.Errorf("spillName code, junk lang = %q", n)
	}
	// A language that sanitizes away entirely falls back to example.md.
	if n := spillName(&Descriptor{Kind: KindCode, Language: "/。/"}); n != "example.md" {
		t.Errorf("spillName code, empty-after-sanitize lang = %q", n)
	}
	// Real language names with punctuation survive (c++, c#).
	if n := spillName(&Descriptor{Kind: KindCode, Language: "c++"}); n != "example.c++.md" {
		t.Errorf("spillName code, c++ = %q", n)
	}
	if n := spillName(&Descriptor{Kind: KindText, Format: FormatHTML}); n != "message.md" {
		t.Errorf("spillName html = %q", n)
	}
	// Code spills as a fenced block (with language); HTML prose is converted to md.
	if p := spillPayload(&Descriptor{Kind: KindCode, Code: "raw", Language: "go"}); p != "```go\nraw\n```" {
		t.Errorf("spillPayload code = %q", p)
	}
	if p := spillPayload(&Descriptor{Kind: KindText, Format: FormatHTML, Text: "<b>x</b>"}); p != "**x**" {
		t.Errorf("spillPayload html->md = %q", p)
	}
}

func TestSpillCodeChunking(t *testing.T) {
	// Code within the budget stays a single fenced block (unchanged behavior).
	if got := spillCodePayload("go", "a\nb\n"); got != "```go\na\nb\n```" {
		t.Fatalf("small code = %q", got)
	}

	// splitCodeForPreview: chunks reconstruct the input and respect the budget.
	line := strings.Repeat("x", 40) + "\n" // 41 bytes/line, no blank lines
	code := strings.Repeat(line, 400)      // ~16.4 KB, forces multiple chunks
	chunks := splitCodeForPreview(code, previewChunkBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != code {
		t.Fatalf("chunks do not reconstruct the code")
	}
	for i, c := range chunks {
		if len(c) > previewChunkBytes {
			t.Fatalf("chunk %d over budget: %d bytes", i, len(c))
		}
	}

	// Prefers to break just after a blank line when one is within reach: fill almost
	// a budget of non-blank lines, drop a blank line, then push past the budget — the
	// first chunk must end at the blank line (its body ends "\n\n").
	var b strings.Builder
	for b.Len() < previewChunkBytes-500 {
		b.WriteString("aaaaaaaa\n")
	}
	b.WriteString("\n") // the blank line the seam should follow
	for i := 0; i < 200; i++ {
		b.WriteString("bbbbbbbb\n")
	}
	pref := splitCodeForPreview(b.String(), previewChunkBytes)
	if !strings.HasSuffix(pref[0], "\n\n") {
		t.Fatalf("first chunk should end just past the blank line, tail = %q",
			pref[0][max(0, len(pref[0])-4):])
	}

	// spillCodePayload joins the blocks with a bare blank-line seam (close fence,
	// empty line, reopen fence) — the form that renders in full on mobile.
	if out := spillCodePayload("py", code); !strings.Contains(out, "```\n\n```py\n") {
		t.Fatal("blocks not joined by a blank-line seam")
	}
}
