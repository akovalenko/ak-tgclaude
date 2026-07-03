package main

import "fmt"

// Kind discriminates the outbound message variants. New kinds (photo, inline
// keyboard, ...) can be added without disturbing callers that switch on Kind.
type Kind string

const (
	// KindText is a text message. Format selects plain or Telegram-HTML.
	KindText Kind = "text"
	// KindCode is a preformatted block with an optional source language. It is
	// rendered as <pre><code class="language-LANG">…</code></pre>, spilling to a
	// document when it exceeds Telegram's message size limit.
	KindCode Kind = "code"
	// KindDocument is a file attachment uploaded from Path.
	KindDocument Kind = "document"
)

// Text formats for KindText.
const (
	FormatPlain = "plain" // no parse_mode (Telegram shows the text verbatim)
	FormatHTML  = "html"  // parse_mode=HTML (the responder supplies valid, escaped HTML)
)

// Descriptor is one outbound Telegram action. The MCP tool handler builds it in
// memory from a send_* tool call, and sendDescriptor renders and delivers it. It
// carries the SEMANTIC message (kind + content) only — never the route: the
// dispatcher pins chat_id/reply_to per invocation (resolved from the capability
// token), so a responder cannot choose a destination.
type Descriptor struct {
	Kind Kind

	// Text (KindText): the message body; Format is "plain" (default) or "html".
	Text   string
	Format string

	// Code (KindCode): the preformatted body and an optional language tag.
	Code     string
	Language string

	// Document (KindDocument): the file to upload (an absolute path, confined to
	// the invocation's outbox by the handler) and an optional display name.
	Path     string
	Filename string

	// Caption is an optional line attached to KindCode / KindDocument.
	Caption string

	// Silent maps to Telegram's disable_notification.
	Silent bool

	// Progress marks an "along the way" message (a status/progress note, not the
	// answer). It is delivered normally but does NOT count toward the delivery guard's
	// send tally — so a pre-reporting responder can narrate its work without blinding
	// the "answer never got sent" check (which keys on zero real sends).
	Progress bool
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
