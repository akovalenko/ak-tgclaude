package main

import "testing"

func TestSessionStorePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSessionStore(dir)
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
	s2, err := LoadSessionStore(dir)
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
	s3, _ := LoadSessionStore(dir)
	if _, ok := s3.SessionID(1); ok {
		t.Errorf("session not cleared after reload")
	}
}
