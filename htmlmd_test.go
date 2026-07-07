package main

import (
	"strings"
	"testing"
)

// TestParseHTMLDeepNestingDoesNotOverflow is the M2 regression: render/textContent
// recurse per tree level, so an adversarial reply of deeply nested tags would once
// overflow the goroutine stack and take the whole dispatcher (all chats) down with a
// fatal runtime throw. parseHTML now caps tree depth, so this completes instead.
func TestParseHTMLDeepNestingDoesNotOverflow(t *testing.T) {
	const n = 300000
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("<b>")
	}
	b.WriteString("deep")
	for i := 0; i < n; i++ {
		b.WriteString("</b>")
	}
	md := htmlToMarkdown(b.String()) // must not overflow the stack / crash
	if !strings.Contains(md, "deep") {
		t.Errorf("innermost content lost after depth cap")
	}
}

// TestHTMLToMarkdown locks the converter's OUTPUT for each tag mapping and every
// parasitic-markdown corner. These assertions are deterministic (string in →
// string out); the SEPARATE empirical question — does the emitted markdown render
// as intended in Telegram's .md-attachment preview — is checked by sending probe
// files, and cases marked EMPIRICAL here may change once that ground truth lands.
func TestHTMLToMarkdown(t *testing.T) {
	bt := "`" // a single backtick, to keep raw-string literals below readable
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},

		// --- tag mappings ---
		{"bold", "<b>x</b>", "**x**"},
		{"strong", "<strong>x</strong>", "**x**"},
		{"italic", "<i>x</i>", "*x*"},
		{"em", "<em>x</em>", "*x*"},
		{"strike", "<s>x</s>", "~~x~~"},                     // GFM strikethrough — confirmed
		{"del", "<del>x</del>", "~~x~~"},                    //
		{"underline raw html", "<u>x</u>", "<u>x</u>"},      // raw <u> renders as underline — confirmed
		{"ins to u", "<ins>y</ins>", "<u>y</u>"},            // <ins> normalised to the confirmed <u>
		{"spoiler drop", "<tg-spoiler>x</tg-spoiler>", "x"}, // no spoiler in md preview — kept as plain text
		{"span unwrap", "<span>x</span>", "x"},
		{"tg-emoji fallback", `<tg-emoji emoji-id="5">👍</tg-emoji>`, "👍"},

		{"nested bi", "<b>a <i>b</i></b>", "**a *b***"},
		{"emphasis trailing space out", "<b>x </b>y", "**x** y"},

		// --- links (corner 2) ---
		{"link sha", `<a href="https://gl/-/blob/abc123/f.go#L5">f.go:5</a>`, "[f.go:5](https://gl/-/blob/abc123/f.go#L5)"},
		{"link label brackets", `<a href="u">a[b]c</a>`, `[a\[b\]c](u)`},
		{"link url spaces angle", `<a href="u v">t</a>`, "[t](<u v>)"},

		// --- inline code (corner 1) ---
		{"code simple", "<code>x</code>", bt + "x" + bt},
		{"code inner backtick", "<code>a" + bt + "b</code>", bt + bt + "a" + bt + "b" + bt + bt},
		{"code leading backtick pad", "<code>" + bt + "x</code>", bt + bt + " " + bt + "x " + bt + bt},

		// --- fenced code (always md) ---
		{"pre plain", "<pre>plain</pre>", bt + bt + bt + "\nplain\n" + bt + bt + bt},
		{"pre lang", `<pre><code class="language-go">fmt.Println()</code></pre>`, bt + bt + bt + "go\nfmt.Println()\n" + bt + bt + bt},
		{"pre inner fence widens", "<pre>a\n" + bt + bt + bt + "\nb</pre>",
			bt + bt + bt + bt + "\na\n" + bt + bt + bt + "\nb\n" + bt + bt + bt + bt},

		// --- line-leading constructs (corner 3) ---
		{"leading hash", "# title", `\# title`},
		{"leading dash", "- item", `\- item`},
		{"leading gt", "> quote", `\> quote`},
		{"ordered dot", "1. first", `1\. first`},
		{"ordered paren", "2) second", `2\) second`},

		// --- inline literals ---
		{"literal star", "a*b", `a\*b`},
		{"literal underscore", "a_b", `a\_b`},
		{"literal brackets", "a[b]", `a\[b\]`},

		// --- entities ---
		{"entity lt", "a &lt; b", `a \< b`},
		{"entity amp bare", "a &amp; b", "a & b"},
		{"entity amp literal", "&amp;amp;", `\&amp;`},

		// --- blockquote ---
		{"blockquote", "<blockquote>hello</blockquote>", "> hello"},
		{"blockquote multiline", "<blockquote>a\nb</blockquote>", "> a\n> b"},

		// --- a compound, realistic answer fragment ---
		{"compound",
			`See <b>foo</b> at <a href="https://gl/-/blob/deadbeef/x.go#L1">x.go:1</a>.`,
			"See **foo** at [x.go:1](https://gl/-/blob/deadbeef/x.go#L1)."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := htmlToMarkdown(c.in); got != c.want {
				t.Errorf("htmlToMarkdown(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}
