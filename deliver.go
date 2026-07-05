package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// sendDescriptor renders one descriptor and delivers it to Telegram on route r,
// spilling an oversized text/code message to a document. It is the shared
// delivery core: the MCP tool handler builds a Descriptor from the tool call and
// calls this to deliver synchronously, returning the Telegram message_id (or the
// send error, which the handler surfaces to the model as a tool error). When up is
// non-nil and a document exceeds its threshold, the file is uploaded and delivered
// as a URL instead of a Telegram attachment (which caps near 50 MB).
func sendDescriptor(ctx context.Context, d *Descriptor, r Route, s Sender, up *uploader) ([]int64, error) {
	if d.Kind == KindDocument {
		if up != nil {
			if info, err := os.Stat(d.Path); err == nil && info.Size() > up.thresholdBytes {
				return oneID(up.deliver(ctx, d, r, s, info.Size()))
			}
		}
		return oneID(s.SendDocument(ctx, r, d.Path, d.Filename, d.Caption, "", d.Silent))
	}
	text, mode := renderMessage(d)
	// Guard: validate Telegram HTML before sending, so the model gets ALL unsupported
	// tags at once (Telegram's own 400 names only the first). Only meaningful in HTML
	// mode; a plain-text message has no tags to check.
	if mode == "HTML" {
		if bad := badTelegramTags(text); len(bad) > 0 {
			return nil, &htmlError{fmt.Sprintf(
				"invalid Telegram HTML — unsupported tag(s): %s. Telegram HTML allows only: %s. "+
					"Use plain newlines and • bullets (not <br>/<p>/<ul>/<li>/<hN>), and <code>/<pre> for code.",
				strings.Join(bad, ", "), strings.Join(telegramTagList, ", "))}
		}
	}
	if fits(text) {
		return oneID(s.SendMessage(ctx, r, text, mode, d.Silent))
	}
	// Oversized. A text message that breaks cleanly at tag-depth-0 newlines goes out
	// as ≤maxSplitParts messages (anchor first); otherwise — or for code, whose
	// rendered <pre> has no depth-0 break — it spills to a document.
	if d.Kind == KindText {
		if parts, ok := splitHTML(text, telegramTextLimit, maxSplitParts); ok {
			return sendParts(ctx, r, parts, mode, d.Silent, s)
		}
	}
	tmp, err := os.CreateTemp("", "ak-tgclaude-spill-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(spillPayload(d)); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	return oneID(s.SendDocument(ctx, r, tmp.Name(), spillName(d), d.Caption, "", d.Silent))
}

// oneID lifts a single-message send result into the []int64 return shape.
func oneID(id int64, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	return []int64{id}, nil
}

// sendParts delivers a split text message as a sequence: the anchor (first part)
// threads to the incoming message via r.ReplyTo, the rest follow as plain messages
// (a reply that quotes a piece is resolved through the transcript's PartOf, not a
// Telegram thread edge). It returns the message ids in order, anchor first. On a
// mid-sequence failure it returns the ids sent so far with the error; the caller
// surfaces the error and records nothing, so already-sent parts are orphaned in the
// chat — a rare, best-effort edge (Telegram seldom rejects mid-run).
func sendParts(ctx context.Context, r Route, parts []string, mode string, silent bool, s Sender) ([]int64, error) {
	ids := make([]int64, 0, len(parts))
	for i, p := range parts {
		pr := r
		if i > 0 {
			pr.ReplyTo = 0 // only the anchor quotes the incoming message
		}
		id, err := s.SendMessage(ctx, pr, p, mode, silent)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
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
