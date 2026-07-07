package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendDescriptorRenders(t *testing.T) {
	f := &fakeSender{}
	route := Route{ChatID: 9, ReplyTo: 3}
	// A document descriptor now has its file opened (O_NOFOLLOW) by the delivery core,
	// so it must point at a real file rather than a made-up path.
	docPath := filepath.Join(t.TempDir(), "x.pdf")
	if err := os.WriteFile(docPath, []byte("PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*Descriptor{
		{Kind: KindText, Text: "hi"},
		{Kind: KindText, Text: "<b>x</b>", Format: FormatHTML},
		{Kind: KindCode, Code: "package main", Language: "go"},
		{Kind: KindDocument, Path: docPath, Filename: "x.pdf"},
	} {
		if _, err := sendDescriptor(context.Background(), d, route, f, nil, overflowSpill); err != nil {
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
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindCode, Code: big, Language: "go"}, Route{ChatID: 1}, f, nil, overflowSpill); err != nil {
		t.Fatal(err)
	}
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindText, Text: big}, Route{ChatID: 1}, f, nil, overflowSpill); err != nil {
		t.Fatal(err)
	}
	calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	// Both spill as Markdown now: code as example.<lang>.md, prose as message.md.
	if calls[0].kind != "document" || calls[0].filename != "example.go.md" {
		t.Errorf("oversized code should spill to example.go.md: %+v", calls[0])
	}
	if calls[1].kind != "document" || calls[1].filename != "message.md" {
		t.Errorf("oversized text should spill to message.md: %+v", calls[1])
	}
}

func TestSendDescriptorOverflowError(t *testing.T) {
	f := &fakeSender{}
	big := strings.Repeat("x", telegramTextLimit+10) // oversized, no depth-0 newline => unsplittable

	// overflow=error: an unsplittable text is refused with a tool error, nothing sent.
	ids, err := sendDescriptor(context.Background(),
		&Descriptor{Kind: KindText, Text: big}, Route{ChatID: 1}, f, nil, overflowError)
	if err == nil {
		t.Fatalf("want an oversize error, got ids=%v", ids)
	}
	var oe *oversizeError
	if !errors.As(err, &oe) {
		t.Fatalf("want *oversizeError, got %T: %v", err, err)
	}
	if n := len(f.snapshot()); n != 0 {
		t.Fatalf("overflow=error must not send anything, got %d sends", n)
	}

	// Code ignores the knob — it always spills, even under overflow=error.
	if _, err := sendDescriptor(context.Background(),
		&Descriptor{Kind: KindCode, Code: big, Language: "go"}, Route{ChatID: 1}, f, nil, overflowError); err != nil {
		t.Fatalf("code should spill under overflow=error, got %v", err)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].kind != "document" || calls[0].filename != "example.go.md" {
		t.Errorf("code should spill to example.go.md under overflow=error: %+v", calls)
	}
}

func TestSendDescriptorSplitsText(t *testing.T) {
	f := &fakeSender{}
	// Two near-limit lines: together over the limit, but the depth-0 newline between
	// them is a clean break, so the message splits into two sends (not a spill).
	line := strings.Repeat("y", telegramTextLimit-5)
	ids, err := sendDescriptor(context.Background(),
		&Descriptor{Kind: KindText, Text: line + "\n" + line}, Route{ChatID: 7, ReplyTo: 42}, f, nil, overflowSpill)
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
