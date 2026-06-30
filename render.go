package main

import (
	"html"
	"strings"
	"unicode/utf8"
)

// telegramTextLimit is Telegram's per-message text cap. A rendered text/code
// message longer than this spills to a document instead.
const telegramTextLimit = 4096

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

// fits reports whether s is within Telegram's text limit.
func fits(s string) bool {
	return utf8.RuneCountInString(s) <= telegramTextLimit
}

// spillName is the document filename for an oversized text/code message.
func spillName(d *Descriptor) string {
	switch d.Kind {
	case KindCode:
		if d.Language != "" {
			return "snippet." + d.Language
		}
		return "snippet.txt"
	default:
		if d.Format == FormatHTML {
			return "message.html"
		}
		return "message.txt"
	}
}

// spillPayload is the raw body attached when a message spills to a document —
// the unwrapped text/code, not the Telegram-HTML rendering.
func spillPayload(d *Descriptor) string {
	if d.Kind == KindCode {
		return d.Code
	}
	return d.Text
}
