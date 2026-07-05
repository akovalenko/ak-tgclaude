package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestBadTelegramTags(t *testing.T) {
	// Clean Telegram HTML (case-insensitive) flags nothing.
	if got := badTelegramTags(`<b>ok</b> <A href="x">l</A> <code>c</code> <pre>p</pre>`); got != nil {
		t.Errorf("clean HTML flagged: %v", got)
	}
	// Unsupported tags are collected, sorted and deduped.
	got := badTelegramTags("<div>x</div><br><ul><li>a</li><li>b</li></ul><p>y</p>")
	want := []string{"br", "div", "li", "p", "ul"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("badTelegramTags = %v, want %v", got, want)
	}
}

func TestSendDescriptorRejectsBadHTML(t *testing.T) {
	// HTML with an unsupported tag is refused BEFORE the send, as an *htmlError, and
	// nothing goes out.
	s := &fakeSender{}
	d := &Descriptor{Kind: KindText, Format: FormatHTML, Text: "<p>hello</p>"}
	_, err := sendDescriptor(context.Background(), d, Route{ChatID: 1}, s, nil, overflowSpill)
	var he *htmlError
	if !errors.As(err, &he) {
		t.Fatalf("want *htmlError, got %v", err)
	}
	if len(s.snapshot()) != 0 {
		t.Errorf("nothing must be sent when the HTML is invalid, got %v", s.snapshot())
	}
	// Clean HTML sends normally.
	s2 := &fakeSender{}
	d2 := &Descriptor{Kind: KindText, Format: FormatHTML, Text: "<b>hi</b>"}
	if _, err := sendDescriptor(context.Background(), d2, Route{ChatID: 1}, s2, nil, overflowSpill); err != nil {
		t.Fatalf("clean HTML should send: %v", err)
	}
	if len(s2.snapshot()) != 1 {
		t.Errorf("clean HTML should have sent one message, got %v", s2.snapshot())
	}
	// Plain text is never HTML-checked (a literal <p> is fine as text).
	s3 := &fakeSender{}
	d3 := &Descriptor{Kind: KindText, Format: FormatPlain, Text: "1 < 2 and <p> as text"}
	if _, err := sendDescriptor(context.Background(), d3, Route{ChatID: 1}, s3, nil, overflowSpill); err != nil {
		t.Fatalf("plain text should send: %v", err)
	}
	if len(s3.snapshot()) != 1 {
		t.Errorf("plain text should have sent one message")
	}
}
