// Package store is the dispatcher's durable, file-backed state: the
// chat→session map and getUpdates offset (Sessions), the per-chat conversation
// transcript with its groomed read-back (Transcripts, Recall), and the
// append-only per-round usage log (UsageLog). The package knows nothing of the
// Telegram API or the responder — callers hand in plain ids, records, and
// paths — and writes are defensive (atomic temp+rename, 0600 files) because
// the state must survive dispatcher restarts.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sessions is the dispatcher's durable state: the getUpdates offset and the
// chat→session map (which Claude Code session answers a given chat, plus that
// chat's persistent working dir and last-used time). It must survive restarts, so
// it is persisted to a JSON file under the state dir.
//
// A future reply-resurrection feature will add a message→session map here. When it
// lands it must resolve a split-message piece to its anchor before the lookup: an
// oversized reply is delivered as several messages but recorded once at the anchor
// (transcript PartOf), so a reply that quotes a piece has to follow PartOf to reach
// the binding. (Recall already follows PartOf — see the tg-recall skill.)
type Sessions struct {
	path string

	// ephemeral keeps the chat→session map in memory only: persist writes just the
	// offset, and a reload starts with no bindings (and scrubs any stale ones from
	// disk). The offset still persists so a restart does not reprocess the
	// getUpdates backlog.
	ephemeral bool

	mu   sync.Mutex
	data storeData
}

type storeData struct {
	Offset   int64                `json:"offset"`
	Sessions map[int64]sessionRec `json:"sessions"` // chat_id -> record
}

// sessionRec is the per-chat record: which Claude session answers the chat, the
// persistent working dir (outbox) reattached across the chat's turns, and when it
// was last used (for TTL eviction). Outbox is absolute; empty means none allocated
// yet. LastUsed is bumped on every SetSession/SetOutbox.
type sessionRec struct {
	SessionID string    `json:"session_id"`
	Outbox    string    `json:"outbox,omitempty"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// UnmarshalJSON accepts BOTH the current object form and the legacy bare-string
// form (an older store persisted the value as just the session id), so upgrading
// an existing sessions.json does not crash the dispatcher on load.
func (r *sessionRec) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' { // legacy: the value was the session id string
		return json.Unmarshal(b, &r.SessionID)
	}
	type raw sessionRec
	var rr raw
	if err := json.Unmarshal(b, &rr); err != nil {
		return err
	}
	*r = sessionRec(rr)
	return nil
}

// LoadSessions opens (or initializes) the store under dir. When ephemeral,
// any persisted chat→session bindings are ignored (each start is fresh) and
// scrubbed from disk at load time, and future writes never carry the map to
// disk; the offset is still loaded/kept.
func LoadSessions(dir string, ephemeral bool) (*Sessions, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	s := &Sessions{
		path:      filepath.Join(dir, "sessions.json"),
		ephemeral: ephemeral,
		data:      storeData{Sessions: map[int64]sessionRec{}},
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	hadBindings := len(s.data.Sessions) > 0
	if s.data.Sessions == nil || ephemeral {
		s.data.Sessions = map[int64]sessionRec{}
	}
	if ephemeral && hadBindings {
		// Scrub the now-ignored bindings from disk at load time (rather than
		// waiting for the first offset write), so the file never lingers with
		// stale sessions once ephemeral mode is on. persist keeps the offset.
		if err := s.persist(); err != nil {
			return nil, fmt.Errorf("scrubbing %s: %w", s.path, err)
		}
	}
	return s, nil
}

// SessionID returns the session bound to chat, if any.
func (s *Sessions) SessionID(chat int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data.Sessions[chat]
	return rec.SessionID, ok
}

// SetSession binds chat to session (preserving the recorded outbox), bumps
// LastUsed, and persists.
func (s *Sessions) SetSession(chat int64, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.data.Sessions[chat]
	rec.SessionID = session
	rec.LastUsed = time.Now()
	s.data.Sessions[chat] = rec
	return s.persist()
}

// Outbox returns the persistent working dir recorded for chat, if one is set.
func (s *Sessions) Outbox(chat int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data.Sessions[chat]
	if !ok || rec.Outbox == "" {
		return "", false
	}
	return rec.Outbox, true
}

// SetOutbox records the persistent working dir for chat (creating the record if
// needed), bumps LastUsed, and persists. An empty path clears the recorded outbox.
func (s *Sessions) SetOutbox(chat int64, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.data.Sessions[chat]
	rec.Outbox = path
	rec.LastUsed = time.Now()
	s.data.Sessions[chat] = rec
	return s.persist()
}

// Outboxes returns a snapshot of every recorded (non-empty) outbox path — used to
// wipe them on clear-all or on shutdown under ephemeral sessions.
func (s *Sessions) Outboxes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, rec := range s.data.Sessions {
		if rec.Outbox != "" {
			out = append(out, rec.Outbox)
		}
	}
	return out
}

// EvictExpired drops every record whose LastUsed is older than ttl before now,
// EXCEPT keep (the chat being served this moment), and returns the outbox paths of
// the dropped records for the caller to remove from disk. ttl <= 0 disables
// eviction. Records with no LastUsed yet (e.g. legacy-migrated) are left alone. The
// WHOLE record goes (session binding included): a chat idle past the TTL is treated
// as a session you would not resume anyway.
func (s *Sessions) EvictExpired(now time.Time, ttl time.Duration, keep int64) ([]string, error) {
	if ttl <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-ttl)
	var evicted []string
	for chat, rec := range s.data.Sessions {
		if chat == keep || rec.LastUsed.IsZero() || !rec.LastUsed.Before(cutoff) {
			continue
		}
		if rec.Outbox != "" {
			evicted = append(evicted, rec.Outbox)
		}
		delete(s.data.Sessions, chat)
	}
	if evicted == nil {
		return nil, nil // nothing expired => nothing to persist
	}
	return evicted, s.persist()
}

// Ephemeral reports whether chat→session bindings live in memory only (so their
// outboxes should be wiped on shutdown rather than left for a restart to resume).
func (s *Sessions) Ephemeral() bool { return s.ephemeral }

// Clear drops the session bound to chat (the /clear path) and persists. The
// caller removes the outbox dir from disk (fetch it via Outbox first).
func (s *Sessions) Clear(chat int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Sessions, chat)
	return s.persist()
}

// ClearAll drops every chat→session binding (the `clear` subcommand), keeping the
// getUpdates offset so the dispatcher does not reprocess the backlog on the next
// start. It returns how many bindings were removed. The caller removes the outbox
// dirs from disk (snapshot them via Outboxes first).
func (s *Sessions) ClearAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.data.Sessions)
	if n == 0 {
		return 0, nil
	}
	s.data.Sessions = map[int64]sessionRec{}
	return n, s.persist()
}

// Offset returns the next getUpdates offset.
func (s *Sessions) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Offset
}

// SetOffset records the next getUpdates offset and persists.
func (s *Sessions) SetOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Offset = offset
	return s.persist()
}

// persist writes the store atomically (temp + rename). Caller holds s.mu. In
// ephemeral mode the chat→session map is omitted, so only the offset reaches
// disk (the in-memory map is left intact for the process lifetime).
func (s *Sessions) persist() error {
	data := s.data
	if s.ephemeral {
		data.Sessions = nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
