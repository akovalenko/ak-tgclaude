package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Kind discriminates the outbound message descriptor variants. New kinds
// (photo, inline keyboard, ...) can be added without breaking older
// descriptors: a reader switches on Kind and leniently ignores fields it does
// not understand.
type Kind string

const (
	// KindText is a text message. Format selects plain or Telegram-HTML.
	KindText Kind = "text"
	// KindCode is a preformatted block with an optional source language. The
	// dispatcher renders it as <pre><code class="language-LANG">…</code></pre>,
	// spilling to a document when it exceeds Telegram's message size limit.
	KindCode Kind = "code"
	// KindDocument is a file attachment uploaded from Path.
	KindDocument Kind = "document"
)

// Text formats for KindText.
const (
	FormatPlain = "plain" // no parse_mode (Telegram shows the text verbatim)
	FormatHTML  = "html"  // parse_mode=HTML (the responder supplies valid, escaped HTML)
)

// descriptorVersion is the on-disk schema version. Bump only on a change that
// older readers cannot tolerate; readers should accept any version they
// understand and treat unknown fields leniently.
const descriptorVersion = 1

// Descriptor is one outbound Telegram action, serialized as a JSON file dropped
// into the outbox spool by `send` and consumed by `dispatch`. It carries the
// SEMANTIC message (kind + content), never the route: the dispatcher pins
// chat_id/reply_to in-process and ignores anything a responder might add here.
type Descriptor struct {
	V    int  `json:"v"`
	Kind Kind `json:"kind"`

	// Text (KindText): the message body; Format is "plain" (default) or "html".
	Text   string `json:"text,omitempty"`
	Format string `json:"format,omitempty"`

	// Code (KindCode): the preformatted body and an optional language tag.
	Code     string `json:"code,omitempty"`
	Language string `json:"language,omitempty"`

	// Document (KindDocument): the file to upload and an optional display name.
	// Path is absolute so it survives the responder's ephemeral cwd; the
	// dispatcher must upload it before that cwd is torn down (a future version
	// may stage the bytes into the spool for full decoupling).
	Path     string `json:"path,omitempty"`
	Filename string `json:"filename,omitempty"`

	// Caption is an optional line attached to KindCode / KindDocument.
	Caption string `json:"caption,omitempty"`

	// Silent maps to Telegram's disable_notification.
	Silent bool `json:"silent,omitempty"`
}

// validate checks that the descriptor's kind-specific fields are populated.
func (d *Descriptor) validate() error {
	switch d.Kind {
	case KindText:
		if d.Text == "" {
			return fmt.Errorf("text message has empty text")
		}
		switch d.Format {
		case "", FormatPlain, FormatHTML:
		default:
			return fmt.Errorf("unknown text format %q (want %q|%q)", d.Format, FormatPlain, FormatHTML)
		}
	case KindCode:
		if d.Code == "" {
			return fmt.Errorf("code message has empty code")
		}
	case KindDocument:
		if d.Path == "" {
			return fmt.Errorf("document message has empty path")
		}
	case "":
		return fmt.Errorf("descriptor has no kind")
	default:
		return fmt.Errorf("unknown descriptor kind %q", d.Kind)
	}
	return nil
}

// dropSeq disambiguates descriptors dropped within the same nanosecond by the
// same process, keeping filenames unique and monotonic.
var dropSeq atomic.Uint64

// Drop writes the descriptor into the outbox directory atomically: it is first
// written to a hidden temp file in the same dir, then renamed into place, so a
// watcher never observes a partial descriptor. Filenames sort in drop order, so
// a consumer that reads them sorted preserves the responder's message order.
func (d *Descriptor) Drop(outbox string) (string, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	if d.V == 0 {
		d.V = descriptorVersion
	}
	body, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshaling descriptor: %w", err)
	}
	name := fmt.Sprintf("%020d-%d-%020d.json", time.Now().UnixNano(), os.Getpid(), dropSeq.Add(1))
	final := filepath.Join(outbox, name)
	tmp := filepath.Join(outbox, "."+name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("writing descriptor: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("publishing descriptor: %w", err)
	}
	return final, nil
}
