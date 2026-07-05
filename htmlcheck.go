package main

import (
	"regexp"
	"sort"
	"strings"
)

// telegramTags are the HTML tags Telegram's parse_mode=HTML accepts. Anything
// else — a <div>, <p>, <br>, <ul>, <li>, <h1>, or a Markdown-ism the model let
// slip as a tag — makes Telegram reject the whole message with a 400. The guard
// below reports them BEFORE the send, and ALL at once, unlike Telegram's 400
// which names only the first offending entity.
var telegramTags = map[string]bool{
	"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true,
	"s": true, "strike": true, "del": true, "a": true, "code": true, "pre": true,
	"span": true, "tg-spoiler": true, "tg-emoji": true, "blockquote": true,
}

// telegramTagList is the whitelist, sorted, for the guard's error message.
var telegramTagList = sortedKeys(telegramTags)

var htmlTagRe = regexp.MustCompile(`</?([a-zA-Z][\w-]*)`)

// badTelegramTags returns the sorted, unique tag names in text that are NOT in the
// Telegram whitelist (nil when the HTML is clean). It matches on tag names only —
// attribute validity (an <a> without href, a bare <span>) is left to Telegram.
func badTelegramTags(text string) []string {
	var seen map[string]bool
	for _, m := range htmlTagRe.FindAllStringSubmatch(text, -1) {
		t := strings.ToLower(m[1])
		if !telegramTags[t] {
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

// htmlError is a pre-send guard failure: the model supplied Telegram HTML with
// unsupported tags. callTool surfaces its message to the model verbatim (not under
// the "Telegram rejected" framing), so the model can fix all the tags at once.
type htmlError struct{ msg string }

func (e *htmlError) Error() string { return e.msg }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
