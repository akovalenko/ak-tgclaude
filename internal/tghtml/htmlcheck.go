package tghtml

import (
	"regexp"
	"sort"
	"strings"
)

// allowedTags are the HTML tags Telegram's parse_mode=HTML accepts. Anything
// else — a <div>, <p>, <br>, <ul>, <li>, <h1>, or a Markdown-ism the model let
// slip as a tag — makes Telegram reject the whole message with a 400. The guard
// below reports them BEFORE the send, and ALL at once, unlike Telegram's 400
// which names only the first offending entity.
var allowedTags = map[string]bool{
	"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true,
	"s": true, "strike": true, "del": true, "a": true, "code": true, "pre": true,
	"span": true, "tg-spoiler": true, "tg-emoji": true, "blockquote": true,
}

// AllowedTags is the whitelist, sorted, for the guard's error message.
var AllowedTags = sortedKeys(allowedTags)

var tagNameRe = regexp.MustCompile(`</?([a-zA-Z][\w-]*)`)

// BadTags returns the sorted, unique tag names in text that are NOT in the
// Telegram whitelist (nil when the HTML is clean). It matches on tag names only —
// attribute validity (an <a> without href, a bare <span>) is left to Telegram.
func BadTags(text string) []string {
	var seen map[string]bool
	for _, m := range tagNameRe.FindAllStringSubmatch(text, -1) {
		t := strings.ToLower(m[1])
		if !allowedTags[t] {
			if seen == nil {
				seen = map[string]bool{}
			}
			seen[t] = true
		}
	}
	if seen == nil {
		return nil
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
