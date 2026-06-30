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

	mu   sync.Mutex
	data storeData
}

type storeData struct {
	Offset   int64            `json:"offset"`
	Sessions map[int64]string `json:"sessions"` // chat_id -> session_id
}

// LoadSessionStore opens (or initializes) the store under dir.
func LoadSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	s := &SessionStore{
		path: filepath.Join(dir, "sessions.json"),
		data: storeData{Sessions: map[int64]string{}},
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
	if s.data.Sessions == nil {
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

// persist writes the store atomically (temp + rename). Caller holds s.mu.
func (s *SessionStore) persist() error {
	b, err := json.Marshal(s.data)
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
