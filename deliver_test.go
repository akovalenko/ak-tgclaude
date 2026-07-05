package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSendDescriptorRenders(t *testing.T) {
	f := &fakeSender{}
	route := Route{ChatID: 9, ReplyTo: 3}
	for _, d := range []*Descriptor{
		{Kind: KindText, Text: "hi"},
		{Kind: KindText, Text: "<b>x</b>", Format: FormatHTML},
		{Kind: KindCode, Code: "package main", Language: "go"},
		{Kind: KindDocument, Path: "/abs/x.pdf", Filename: "x.pdf"},
	} {
		if _, err := sendDescriptor(context.Background(), d, route, f, nil); err != nil {
			t.Fatalf("sendDescriptor(%+v): %v", d, err)
		}
	}
	calls := f.snapshot()
	if len(calls) != 4 {
		t.Fatalf("want 4 calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].kind != "message" || calls[0].text != "hi" || calls[0].mode != "" {
		t.Errorf("text plain wrong: %+v", calls[0])
	}
	if calls[1].mode != "HTML" || calls[1].text != "<b>x</b>" {
		t.Errorf("text html wrong: %+v", calls[1])
	}
	if calls[2].mode != "HTML" || !strings.Contains(calls[2].text, `class="language-go"`) {
		t.Errorf("code wrong: %+v", calls[2])
	}
	if calls[3].kind != "document" || calls[3].filename != "x.pdf" {
		t.Errorf("document wrong: %+v", calls[3])
	}
	if calls[0].route != route {
		t.Errorf("route not passed through: %+v", calls[0].route)
	}
}

func TestSendDescriptorSpillsOversized(t *testing.T) {
	f := &fakeSender{}
	big := strings.Repeat("x", telegramTextLimit+10)
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindCode, Code: big, Language: "go"}, Route{ChatID: 1}, f, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindText, Text: big}, Route{ChatID: 1}, f, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	if calls[0].kind != "document" || calls[0].filename != "snippet.go" {
		t.Errorf("oversized code should spill to snippet.go: %+v", calls[0])
	}
	if calls[1].kind != "document" || calls[1].filename != "message.txt" {
		t.Errorf("oversized text should spill to message.txt: %+v", calls[1])
	}
}

func TestSendDescriptorSplitsText(t *testing.T) {
	f := &fakeSender{}
	// Two near-limit lines: together over the limit, but the depth-0 newline between
	// them is a clean break, so the message splits into two sends (not a spill).
	line := strings.Repeat("y", telegramTextLimit-5)
	ids, err := sendDescriptor(context.Background(),
		&Descriptor{Kind: KindText, Text: line + "\n" + line}, Route{ChatID: 7, ReplyTo: 42}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 message ids, got %d: %v", len(ids), ids)
	}
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 message sends, got %d", len(calls))
	}
	for i, c := range calls {
		if c.kind != "message" {
			t.Errorf("part %d should be a message, not %q", i, c.kind)
		}
	}
	// Only the anchor quotes the incoming message; the piece is a plain follow-up.
	if calls[0].route.ReplyTo != 42 {
		t.Errorf("anchor should reply to the incoming message (42), got %d", calls[0].route.ReplyTo)
	}
	if calls[1].route.ReplyTo != 0 {
		t.Errorf("piece should not reply (0), got %d", calls[1].route.ReplyTo)
	}
}

func TestDeliveryError(t *testing.T) {
	if got := deliveryError(&APIError{Code: 400, Description: "bad html"}); got != "bad html" {
		t.Errorf("APIError should surface Description, got %q", got)
	}
	if got := deliveryError(errors.New("network down")); got != "network down" {
		t.Errorf("plain error should surface its text, got %q", got)
	}
}
