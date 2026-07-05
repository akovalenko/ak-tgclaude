package main

import (
	"html"
	"regexp"
	"strings"
)

// htmlToMarkdown converts a Telegram-HTML fragment (the whitelist subset that
// htmlcheck.go enforces) into Markdown for a spilled .md document. The target is
// NOT CommonMark in the abstract but Telegram's own rendering of an attached .md
// file — a CommonMark/GFM-flavoured renderer with possible deviations. Anything
// marked EMPIRICAL below is a mapping that standard Markdown cannot express and
// must be checked against the real Telegram render (send a probe .md via
// send.sh doc), then adjusted here and in the test table.
//
// The hard part is not the tag mapping (b→**, code→backtick, …) but escaping
// PARASITIC Markdown in literal text: a literal backtick, asterisk, or a line
// that happens to start with '#'/'-'/'>' would otherwise become syntax. The
// mdWriter below does that escaping while tracking whether it sits at the start
// of an output line (leading-char constructs only fire there).
func htmlToMarkdown(s string) string {
	w := newMdWriter()
	w.renderChildren(parseHTML(s))
	return strings.TrimRight(w.sb.String(), "\n")
}

// --- HTML tokenizer → node tree -------------------------------------------

// node is a text leaf (isText) or an element with children. The root is an
// element with an empty name.
type htmlNode struct {
	isText   bool
	text     string // raw, still entity-encoded, when isText
	name     string // lowercased tag name
	attrs    map[string]string
	children []*htmlNode
}

// tagRe matches a single start/end tag. The attribute run tolerates quoted
// values that themselves contain '>' (e.g. href="a>b") but never crosses an
// unquoted '>'.
var tagRe = regexp.MustCompile(`<(/?)([a-zA-Z][\w-]*)((?:"[^"]*"|'[^']*'|[^<>])*)>`)
var attrRe = regexp.MustCompile(`([\w-]+)(?:\s*=\s*("[^"]*"|'[^']*'|[^\s"'<>]+))?`)

// parseHTML builds a lenient tree from a validated Telegram-HTML fragment.
// Mismatched/unclosed tags are tolerated (the guard already vetted tag names).
func parseHTML(s string) *htmlNode {
	root := &htmlNode{}
	stack := []*htmlNode{root}
	top := func() *htmlNode { return stack[len(stack)-1] }

	pos := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > pos {
			t := &htmlNode{isText: true, text: s[pos:m[0]]}
			top().children = append(top().children, t)
		}
		pos = m[1]

		closing := m[3] > m[2] // group 1 "/" is non-empty
		name := strings.ToLower(s[m[4]:m[5]])
		attrsStr := s[m[6]:m[7]]

		if closing {
			for i := len(stack) - 1; i >= 1; i-- {
				if stack[i].name == name {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		el := &htmlNode{name: name, attrs: parseAttrs(attrsStr)}
		top().children = append(top().children, el)
		if !strings.HasSuffix(strings.TrimSpace(attrsStr), "/") {
			stack = append(stack, el) // not self-closed → open a scope
		}
	}
	if pos < len(s) {
		top().children = append(top().children, &htmlNode{isText: true, text: s[pos:]})
	}
	return root
}

func parseAttrs(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out map[string]string
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		k := strings.ToLower(m[1])
		v := m[2]
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			v = v[1 : len(v)-1]
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// textContent flattens an element's descendants to their literal (entity-decoded)
// text — used for code/pre bodies, where inner markup is irrelevant.
func textContent(n *htmlNode) string {
	if n.isText {
		return html.UnescapeString(n.text)
	}
	var b strings.Builder
	for _, c := range n.children {
		b.WriteString(textContent(c))
	}
	return b.String()
}

// --- Markdown writer -------------------------------------------------------

// mdWriter accumulates Markdown while tracking the number of trailing newlines
// (so block elements can guarantee a blank-line separation) and whether it is at
// the start of a line (so leading-char escaping fires only there).
type mdWriter struct {
	sb          strings.Builder
	trailingNL  int
	atLineStart bool
}

func newMdWriter() *mdWriter { return &mdWriter{atLineStart: true} }

// put writes markup or already-escaped text verbatim, maintaining bookkeeping.
func (w *mdWriter) put(s string) {
	if s == "" {
		return
	}
	w.sb.WriteString(s)
	nl := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		nl++
	}
	if nl == len(s) {
		w.trailingNL += nl
	} else {
		w.trailingNL = nl
	}
	w.atLineStart = w.trailingNL > 0
}

// ensureBlankLine guarantees the buffer ends with a blank line (two newlines),
// so a following block element is its own Markdown block. No-op at buffer start.
func (w *mdWriter) ensureBlankLine() {
	if w.sb.Len() == 0 {
		return
	}
	for w.trailingNL < 2 {
		w.put("\n")
	}
}

// text writes literal prose, escaping parasitic Markdown. Line-leading constructs
// (#, >, -, +, ordered "1.") are escaped only at the start of an output line.
func (w *mdWriter) text(s string) {
	parts := strings.Split(s, "\n")
	for i, part := range parts {
		if i > 0 {
			w.put("\n")
		}
		if part == "" {
			continue
		}
		rest := part
		if w.atLineStart {
			rest = w.emitLeading(part)
		}
		w.inlineEscape(rest)
	}
}

// emitLeading escapes a line-leading block marker (heading/quote/list/thematic
// break/ordered item) at the front of line, writes the escaped prefix, and
// returns the remaining text for inline escaping.
func (w *mdWriter) emitLeading(line string) string {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	w.put(line[:i]) // preserve indentation as-is
	rest := line[i:]
	if rest == "" {
		return ""
	}
	switch rest[0] {
	case '#', '>', '+', '-': // heading / blockquote / bullet / thematic break / setext '-'
		w.put(`\`)
		w.put(rest[:1])
		return rest[1:]
	}
	// ordered list: digits followed by '.' or ')'
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j > 0 && j < len(rest) && (rest[j] == '.' || rest[j] == ')') {
		w.put(rest[:j]) // the digits are safe
		w.put(`\`)
		w.put(rest[j : j+1])
		return rest[j+1:]
	}
	return rest
}

var entityLikeRe = regexp.MustCompile(`^&#?[0-9a-zA-Z]+;`)

// inlineEscape escapes the characters that trigger inline Markdown anywhere on a
// line. Escaping is byte-wise: every target is ASCII, and UTF-8 continuation
// bytes are ≥0x80 so they never collide.
func (w *mdWriter) inlineEscape(s string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '`', '*', '_', '[', ']', '~', '<':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '&':
			// Only escape an '&' that would start an entity reference; a bare '&'
			// renders literally, so leave the common case unescaped.
			if entityLikeRe.MatchString(s[i:]) {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	w.put(b.String())
}

// --- tree → Markdown -------------------------------------------------------

func (w *mdWriter) renderChildren(n *htmlNode) {
	for _, c := range n.children {
		w.render(c)
	}
}

func (w *mdWriter) render(n *htmlNode) {
	if n.isText {
		w.text(html.UnescapeString(n.text))
		return
	}
	switch n.name {
	case "b", "strong":
		w.wrapInline("**", n)
	case "i", "em":
		w.wrapInline("*", n)
	case "s", "strike", "del":
		w.wrapInline("~~", n) // EMPIRICAL: GFM strikethrough; renders literally if the target is CommonMark-only.
	case "u", "ins":
		// EMPIRICAL: Markdown has no underline; keep the text unformatted rather
		// than emit a construct the renderer would show literally. Revisit if
		// Telegram honours raw <u> or treats __x__ as underline.
		w.renderChildren(n)
	case "span", "tg-spoiler":
		// EMPIRICAL: no Markdown spoiler; drop the spoiler, keep the text. A plain
		// <span> is just an unwrap. Revisit if the renderer honours ||spoiler||.
		w.renderChildren(n)
	case "tg-emoji":
		w.renderChildren(n) // fall back to the inner emoji character
	case "a":
		w.renderLink(n)
	case "code":
		w.renderInlineCode(n)
	case "pre":
		w.renderPre(n)
	case "blockquote":
		w.renderBlockquote(n)
	default:
		w.renderChildren(n) // unknown container → unwrap
	}
}

// wrapInline emphasises children, moving any surrounding whitespace OUTSIDE the
// delimiters — CommonMark rejects "** x **" (space adjacent to the marker), so
// the spaces must sit outside.
func (w *mdWriter) wrapInline(delim string, n *htmlNode) {
	sub := &mdWriter{} // mid-line: not at line start
	sub.renderChildren(n)
	s := sub.sb.String()
	lead := len(s) - len(strings.TrimLeft(s, " \t"))
	trail := len(s) - len(strings.TrimRight(s, " \t"))
	if lead+trail >= len(s) { // all whitespace: nothing to emphasise
		w.put(s)
		return
	}
	w.put(s[:lead])
	w.put(delim)
	w.put(s[lead : len(s)-trail])
	w.put(delim)
	w.put(s[len(s)-trail:])
}

func (w *mdWriter) renderLink(n *htmlNode) {
	w.put("[")
	w.renderChildren(n) // label; text() escapes any literal [ ] inside
	w.put("](")
	w.put(escapeURL(n.attrs["href"]))
	w.put(")")
}

// escapeURL renders an <a href> value as a CommonMark link destination. A URL
// with spaces or unbalanced parens goes in angle-bracket form; otherwise bare,
// with parens/spaces backslash-escaped as a fallback.
func escapeURL(u string) string {
	u = html.UnescapeString(u)
	if u == "" {
		return ""
	}
	if (strings.ContainsAny(u, " \t") || !balancedParens(u)) && !strings.ContainsAny(u, "<>\n") {
		return "<" + u + ">"
	}
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		if c := u[i]; c == '(' || c == ')' || c == ' ' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(u[i])
	}
	return b.String()
}

func balancedParens(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// renderInlineCode writes an inline code span. The delimiter is a backtick run
// LONGER than any run inside the content (backslash-escaping does not work in a
// code span); if the content starts or ends with a backtick, a space pad lets
// the renderer strip it. This is corner (1) of the design.
func (w *mdWriter) renderInlineCode(n *htmlNode) {
	c := textContent(n)
	delim := strings.Repeat("`", longestBacktickRun(c)+1)
	pad := ""
	if len(c) > 0 && (c[0] == '`' || c[len(c)-1] == '`') {
		pad = " "
	}
	w.put(delim)
	w.put(pad)
	w.put(c)
	w.put(pad)
	w.put(delim)
}

// renderPre writes a fenced code block. The fence is ≥3 backticks and longer
// than any backtick run inside. A <code class="language-X"> child supplies the
// info string. The code body is verbatim — inside a fence, only the closing
// fence is significant (why "code is always safe as md").
func (w *mdWriter) renderPre(n *htmlNode) {
	lang := preLanguage(n)
	body := textContent(n)
	fence := strings.Repeat("`", max(3, longestBacktickRun(body)+1))

	w.ensureBlankLine()
	w.put(fence)
	w.put(lang)
	w.put("\n")
	w.put(body)
	if !strings.HasSuffix(body, "\n") {
		w.put("\n")
	}
	w.put(fence)
	w.ensureBlankLine()
}

// preLanguage extracts the language tag from a <code class="language-X"> child.
func preLanguage(n *htmlNode) string {
	for _, c := range n.children {
		if c.name == "code" {
			cls := c.attrs["class"]
			for _, f := range strings.Fields(cls) {
				if s, ok := strings.CutPrefix(f, "language-"); ok {
					return s
				}
			}
		}
	}
	return ""
}

// renderBlockquote renders children, then prefixes every line with "> ".
func (w *mdWriter) renderBlockquote(n *htmlNode) {
	sub := newMdWriter()
	sub.renderChildren(n)
	inner := strings.TrimRight(sub.sb.String(), "\n")

	w.ensureBlankLine()
	for i, line := range strings.Split(inner, "\n") {
		if i > 0 {
			w.put("\n")
		}
		if line == "" {
			w.put(">")
		} else {
			w.put("> ")
			w.put(line)
		}
	}
	w.ensureBlankLine()
}

func longestBacktickRun(s string) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}
