package tghtml

import (
	"strings"
	"testing"
)

func TestSplitAtDepthZero(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"plain lines", "a\nb\nc", []string{"a", "b", "c"}},
		{"no newline", "abc", []string{"abc"}},
		{"newline inside element not a break", "<b>aa\nbb</b>\ncc", []string{"<b>aa\nbb</b>", "cc"}},
		{"pre keeps its newlines", "<pre>x\ny\nz</pre>", []string{"<pre>x\ny\nz</pre>"}},
		{"blockquote whole then break", "<blockquote>a\nb</blockquote>\nc", []string{"<blockquote>a\nb</blockquote>", "c"}},
		{"nested depth", "<b><i>x\ny</i></b>\nz", []string{"<b><i>x\ny</i></b>", "z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitAtDepthZero(c.in)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("splitAtDepthZero(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSplitHTML(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		limit     int
		maxParts  int
		wantParts []string
		wantOK    bool
	}{
		{"fits in one", "short", 4096, 4, []string{"short"}, true},
		{"packs greedily", "aaaa\nbbbb\ncccc", 9, 4, []string{"aaaa\nbbbb", "cccc"}, true},
		{"breaks only at depth 0", "<b>aa\nbb</b>\ncc", 13, 4, []string{"<b>aa\nbb</b>", "cc"}, true},
		{"indivisible element overflows", "<b>aa\nbb</b>\ncc", 11, 4, nil, false},
		{"single long line overflows", "xxxxxxxxxx", 5, 4, nil, false},
		{"code-like pre overflows not splits", "<pre>x\ny\nz</pre>", 5, 4, nil, false},
		{"too many parts overflows", "a\nb\nc\nd\ne", 1, 4, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts, ok := Split(c.text, c.limit, c.maxParts)
			if ok != c.wantOK {
				t.Fatalf("Split ok = %v, want %v (parts=%q)", ok, c.wantOK, parts)
			}
			if !ok {
				return
			}
			if strings.Join(parts, "|") != strings.Join(c.wantParts, "|") {
				t.Errorf("Split(%q) parts = %q, want %q", c.text, parts, c.wantParts)
			}
			for i, p := range parts {
				if UTF16Len(p) > c.limit {
					t.Errorf("part %d does not fit the limit: %q", i, p)
				}
			}
			// Reconstruction invariant: rejoining chunks with the boundary newline
			// yields the original text (split only removed depth-0 separators).
			if got := strings.Join(parts, "\n"); got != c.text {
				t.Errorf("rejoined = %q, want original %q", got, c.text)
			}
		})
	}
}
