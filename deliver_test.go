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
		if _, err := sendDescriptor(context.Background(), d, route, f); err != nil {
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
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindCode, Code: big, Language: "go"}, Route{ChatID: 1}, f); err != nil {
		t.Fatal(err)
	}
	if _, err := sendDescriptor(context.Background(), &Descriptor{Kind: KindText, Text: big}, Route{ChatID: 1}, f); err != nil {
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

func TestDeliveryError(t *testing.T) {
	if got := deliveryError(&APIError{Code: 400, Description: "bad html"}); got != "bad html" {
		t.Errorf("APIError should surface Description, got %q", got)
	}
	if got := deliveryError(errors.New("network down")); got != "network down" {
		t.Errorf("plain error should surface its text, got %q", got)
	}
}
