package main

import (
	"context"
	"errors"
	"os"
)

// sendDescriptor renders one descriptor and delivers it to Telegram on route r,
// spilling an oversized text/code message to a document. It is the shared
// delivery core: the MCP tool handler builds a Descriptor from the tool call and
// calls this to deliver synchronously, returning the Telegram message_id (or the
// send error, which the handler surfaces to the model as a tool error).
func sendDescriptor(ctx context.Context, d *Descriptor, r Route, s Sender) (int64, error) {
	if d.Kind == KindDocument {
		return s.SendDocument(ctx, r, d.Path, d.Filename, d.Caption, "", d.Silent)
	}
	text, mode := renderMessage(d)
	if fits(text) {
		return s.SendMessage(ctx, r, text, mode, d.Silent)
	}
	tmp, err := os.CreateTemp("", "ak-tgclaude-spill-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(spillPayload(d)); err != nil {
		tmp.Close()
		return 0, err
	}
	tmp.Close()
	return s.SendDocument(ctx, r, tmp.Name(), spillName(d), d.Caption, "", d.Silent)
}

// deliveryError returns the human-readable reason a send failed, for surfacing
// to the model as a tool error: the clean Telegram description when it is an
// *APIError (what the responder can act on — e.g. fix bad HTML), else the full
// error text.
func deliveryError(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Description
	}
	return err.Error()
}
