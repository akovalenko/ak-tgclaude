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

	if n := spillName(&Descriptor{Kind: KindCode, Language: "py"}); n != "snippet.py" {
		t.Errorf("spillName code = %q", n)
	}
	if n := spillName(&Descriptor{Kind: KindCode}); n != "snippet.txt" {
		t.Errorf("spillName code no-lang = %q", n)
	}
	if n := spillName(&Descriptor{Kind: KindText, Format: FormatHTML}); n != "message.html" {
		t.Errorf("spillName html = %q", n)
	}
	if p := spillPayload(&Descriptor{Kind: KindCode, Code: "raw"}); p != "raw" {
		t.Errorf("spillPayload code = %q", p)
	}
}
