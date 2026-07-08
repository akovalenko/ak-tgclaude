package tghtml

import (
	"reflect"
	"testing"
)

func TestBadTags(t *testing.T) {
	// Clean Telegram HTML (case-insensitive) flags nothing.
	if got := BadTags(`<b>ok</b> <A href="x">l</A> <code>c</code> <pre>p</pre>`); got != nil {
		t.Errorf("clean HTML flagged: %v", got)
	}
	// Unsupported tags are collected, sorted and deduped.
	got := BadTags("<div>x</div><br><ul><li>a</li><li>b</li></ul><p>y</p>")
	want := []string{"br", "div", "li", "p", "ul"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BadTags = %v, want %v", got, want)
	}
}
