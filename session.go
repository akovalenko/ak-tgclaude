package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SessionStore is the dispatcher's durable state: the getUpdates offset and the
// chat→session map (which Claude Code session answers a given chat). It must
// survive restarts, so it is persisted to a JSON file under the state dir.
//
// A future reply-resurrection feature will add a message→session map here.
type SessionStore struct {
	path string

	// ephemeral keeps the chat→session map in memory only: persist writes just the
	// offset, and a reload starts with no bindings. The offset still persists so a
	// restart does not reprocess the getUpdates backlog.
	ephemeral bool

	mu   sync.Mutex
	data storeData
}

type storeData struct {
	Offset   int64            `json:"offset"`
	Sessions map[int64]string `json:"sessions"` // chat_id -> session_id
}

// LoadSessionStore opens (or initializes) the store under dir. When ephemeral,
// any persisted chat→session bindings are ignored (each start is fresh) and
// future writes never carry the map to disk; the offset is still loaded/kept.
func LoadSessionStore(dir string, ephemeral bool) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	s := &SessionStore{
		path:      filepath.Join(dir, "sessions.json"),
		ephemeral: ephemeral,
		data:      storeData{Sessions: map[int64]string{}},
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
	if s.data.Sessions == nil || ephemeral {
		s.data.Sessions = map[int64]string{}
	}
	return s, nil
}

// SessionID returns the session bound to chat, if any.
func (s *SessionStore) SessionID(chat int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.data.Sessions[chat]
	return sid, ok
}

// SetSession binds chat to session and persists.
func (s *SessionStore) SetSession(chat int64, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Sessions[chat] = session
	return s.persist()
}

// Clear drops the session bound to chat (the /clear path) and persists.
func (s *SessionStore) Clear(chat int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Sessions, chat)
	return s.persist()
}

// ClearAll drops every chat→session binding (the `clear` subcommand), keeping
// the getUpdates offset so the dispatcher does not reprocess the backlog on the
// next start. It returns how many bindings were removed.
func (s *SessionStore) ClearAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.data.Sessions)
	if n == 0 {
		return 0, nil
	}
	s.data.Sessions = map[int64]string{}
	return n, s.persist()
}

// Offset returns the next getUpdates offset.
func (s *SessionStore) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Offset
}

// SetOffset records the next getUpdates offset and persists.
func (s *SessionStore) SetOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Offset = offset
	return s.persist()
}

// persist writes the store atomically (temp + rename). Caller holds s.mu. In
// ephemeral mode the chat→session map is omitted, so only the offset reaches
// disk (the in-memory map is left intact for the process lifetime).
func (s *SessionStore) persist() error {
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
