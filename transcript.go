package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// TranscriptStore is the durable, per-chat record of the FAQ conversation: every
// user message and every model reply, appended as compact JSONL day-files under
// <root>/<chat_id>/<YYYY-MM-DD>.jsonl, plus a per-chat meta.json. It lives OUTSIDE
// the responder outbox (so it survives the session-TTL wipe and restarts) and is
// read back by the responder for context recall (scoped to the chat, or the whole
// root for the owner). Only the dispatcher writes it.
//
// The store is internally synchronized: the bot-side append runs in the MCP
// server's request goroutine, not the per-chat chatWorkers goroutine that drives
// the user-side append, so the two race within a chat. The day-file O_APPEND is
// atomic on its own, but meta.json is a read-modify-write, so a mutex guards both.
type TranscriptStore struct {
	root string
	mu   sync.Mutex
}

// TranscriptRecord is one turn. Field order is deliberate: MsgID marshals FIRST so
// every compact line begins `{"msg_id":N,` — which makes the recall skill's
// anchored grep (`"msg_id":N[,}]`) a reliable point lookup.
type TranscriptRecord struct {
	MsgID int64 `json:"msg_id"`
	// TS marshals to RFC3339 in the host's local zone (offset included), e.g.
	// "2026-07-04T09:14:07+03:00" — readable local time for the model AND a fully
	// determined instant any DB ingests as timestamptz. Append truncates it to whole
	// seconds (Telegram's precision), so bot-side time.Now() nanos don't leak in.
	TS      time.Time `json:"ts"`
	Role    string    `json:"role"` // "user" | "bot"
	ReplyTo int64     `json:"reply_to,omitempty"`
	// PartOf links a split-message piece to its anchor. An oversized reply is
	// delivered as several messages (see splitHTML), but only the FIRST — the anchor
	// — carries the full Text; each later piece is a light stub {msg_id, part_of:
	// anchor} with empty Text, so the answer is stored once. A reader that lands on a
	// piece (recall, or a reply that threads to it) follows PartOf to the anchor for
	// the text. Zero (omitted) for a normal, unsplit message. Placed after MsgID so
	// the recall grep's `{"msg_id":N,` anchor is unaffected.
	PartOf int64              `json:"part_of,omitempty"`
	Text   string             `json:"text"`
	Attach []TranscriptAttach `json:"attach,omitempty"`
	// User/Name identify the AUTHOR of a turn — needed in a GROUP transcript, where one
	// chat carries many speakers and meta.json holds only the latest. Omitted (0/"") on
	// the private side, where the single chat partner is implied, so private-chat lines
	// keep their old shape. User is a Telegram user id. Declared after MsgID so the
	// recall grep's `{"msg_id":N,` anchor is unaffected.
	User int64  `json:"user,omitempty"`
	Name string `json:"name,omitempty"`
}

// TranscriptAttach records that a turn carried a file — metadata only, never the
// bytes (the design stores text; attachments are noted, not saved).
type TranscriptAttach struct {
	Kind string `json:"kind"` // "photo" | "document"
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
	Mime string `json:"mime,omitempty"`
}

// ChatIdentity is who the chat is, captured from incoming user messages so the
// owner can tell a numeric chat_id apart from a person. Set on user appends only
// (the bot side carries no user identity).
type ChatIdentity struct {
	Username  string
	FirstName string
}

// transcriptMeta is the per-chat meta.json: identity plus first/last-seen and
// per-role counts, so a reader orients on a chat without scanning its day-files.
type transcriptMeta struct {
	Username  string    `json:"username,omitempty"`
	FirstName string    `json:"first_name,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	UserCount int64     `json:"user_count"`
	BotCount  int64     `json:"bot_count"`
}

// NewTranscriptStore returns a store rooted at root. It does not create root — the
// chat directory is made lazily on the first Append, so an idle feature touches no
// disk.
func NewTranscriptStore(root string) *TranscriptStore {
	return &TranscriptStore{root: root}
}

// Append writes rec to chat's day-file (chosen from rec.TS in host-local time) and
// updates chat's meta.json. ident, when non-nil (user side), refreshes the recorded
// username/first_name. A zero rec.TS falls back to now.
func (s *TranscriptStore) Append(chatID int64, rec TranscriptRecord, ident *ChatIdentity) error {
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	rec.TS = rec.TS.Truncate(time.Second) // Telegram is second-precision; drop time.Now() nanos
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.root, strconv.FormatInt(chatID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("transcript dir %s: %w", dir, err)
	}

	// Compact JSONL line (json.Marshal emits no spaces, so the grep anchor holds).
	line, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	day := filepath.Join(dir, rec.TS.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(day, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", day, err)
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("append %s: %w", day, werr)
	}
	if cerr != nil {
		return fmt.Errorf("close %s: %w", day, cerr)
	}

	return s.updateMeta(dir, rec, ident)
}

// updateMeta read-modify-writes the chat's meta.json. Caller holds s.mu.
func (s *TranscriptStore) updateMeta(dir string, rec TranscriptRecord, ident *ChatIdentity) error {
	path := filepath.Join(dir, "meta.json")
	var m transcriptMeta
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if m.FirstSeen.IsZero() {
		m.FirstSeen = rec.TS
	}
	m.LastSeen = rec.TS
	switch rec.Role {
	case "user":
		m.UserCount++
	case "bot":
		m.BotCount++
	}
	if ident != nil {
		m.Username = ident.Username
		m.FirstName = ident.FirstName
	}

	// Pretty-print (meta is human-read) and write atomically (temp + rename, 0600),
	// mirroring SessionStore.persist.
	b, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
