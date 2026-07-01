package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStorePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSessionStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.SessionID(1); ok {
		t.Errorf("fresh store should have no sessions")
	}
	if err := s.SetSession(1, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffset(123); err != nil {
		t.Fatal(err)
	}

	// Reload from the same path.
	s2, err := LoadSessionStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if sid, ok := s2.SessionID(1); !ok || sid != "sess-a" {
		t.Errorf("session not persisted: %q ok=%v", sid, ok)
	}
	if s2.Offset() != 123 {
		t.Errorf("offset not persisted: %d", s2.Offset())
	}

	if err := s2.Clear(1); err != nil {
		t.Fatal(err)
	}
	s3, _ := LoadSessionStore(dir, false)
	if _, ok := s3.SessionID(1); ok {
		t.Errorf("session not cleared after reload")
	}
}

func TestEphemeralSessionsKeepOffsetButNotBindings(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSessionStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(1, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffset(99); err != nil {
		t.Fatal(err)
	}
	// The binding is live in memory for this process...
	if sid, ok := s.SessionID(1); !ok || sid != "sess-a" {
		t.Errorf("in-memory binding lost: %q ok=%v", sid, ok)
	}
	// ...but a reload (ephemeral or not) sees no bindings, while the offset survives.
	s2, _ := LoadSessionStore(dir, false)
	if _, ok := s2.SessionID(1); ok {
		t.Errorf("ephemeral binding should not have been persisted")
	}
	if s2.Offset() != 99 {
		t.Errorf("offset should persist even in ephemeral mode: %d", s2.Offset())
	}
}

func TestEphemeralLoadScrubsDiskBindings(t *testing.T) {
	dir := t.TempDir()
	// Seed a persistent store with bindings + an offset.
	s, _ := LoadSessionStore(dir, false)
	if err := s.SetSession(1, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffset(88); err != nil {
		t.Fatal(err)
	}

	// Loading ephemeral must scrub the on-disk bindings immediately — before any
	// SetOffset — while keeping the offset.
	if _, err := LoadSessionStore(dir, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d storeData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Sessions) != 0 {
		t.Errorf("ephemeral load should scrub disk bindings, got %v", d.Sessions)
	}
	if d.Offset != 88 {
		t.Errorf("offset should survive the load-time scrub: %d", d.Offset)
	}
}

func TestClearAllDropsBindingsKeepsOffset(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadSessionStore(dir, false)
	if err := s.SetSession(1, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(2, "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffset(7); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClearAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("ClearAll returned %d, want 2", n)
	}
	// A second clear is a no-op.
	if n, _ := s.ClearAll(); n != 0 {
		t.Errorf("second ClearAll should remove 0, got %d", n)
	}
	s2, _ := LoadSessionStore(dir, false)
	if _, ok := s2.SessionID(1); ok {
		t.Errorf("binding survived ClearAll after reload")
	}
	if s2.Offset() != 7 {
		t.Errorf("offset should survive ClearAll: %d", s2.Offset())
	}
}
