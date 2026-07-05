package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedTS builds a deterministic timestamp in UTC so day-file names don't depend on
// the host time zone in tests (real records carry host-local times).
func fixedTS(y int, mo time.Month, d, h int) time.Time {
	return time.Date(y, mo, d, h, 0, 0, 0, time.UTC)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if s := sc.Text(); s != "" {
			lines = append(lines, s)
		}
	}
	return lines
}

func TestTranscriptAppendCreatesDayFileAndMeta(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	rec := TranscriptRecord{MsgID: 10, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "как перезапустить asmo?"}
	if err := s.Append(42, rec, &ChatIdentity{Username: "anton", FirstName: "Anton"}); err != nil {
		t.Fatal(err)
	}

	day := filepath.Join(root, "42", "2026-07-04.jsonl")
	lines := readLines(t, day)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var got TranscriptRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("bad JSONL: %v", err)
	}
	if got.MsgID != 10 || got.Role != "user" || got.Text != rec.Text {
		t.Errorf("record round-trip mismatch: %+v", got)
	}

	var m transcriptMeta
	b, err := os.ReadFile(filepath.Join(root, "42", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Username != "anton" || m.FirstName != "Anton" {
		t.Errorf("identity not recorded: %+v", m)
	}
	if !m.FirstSeen.Equal(m.LastSeen) || m.UserCount != 1 {
		t.Errorf("meta counts wrong: %+v", m)
	}
}

func TestTranscriptGroupMetaNamesGroupNotSpeaker(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	// A group turn: the record carries the speaker (User/Name/Username), but the
	// chat's meta.json must name the GROUP itself, not whoever spoke.
	rec := TranscriptRecord{
		MsgID: 1, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "hi",
		User: 7, Name: "Anton", Username: "ak",
	}
	if err := s.Append(-100, rec, &ChatIdentity{Type: "supergroup", Title: "ОСУТ чат", Username: "osut"}); err != nil {
		t.Fatal(err)
	}
	m := readMeta(filepath.Join(root, "-100"))
	if m == nil {
		t.Fatal("group meta.json missing")
	}
	if m.Title != "ОСУТ чат" || m.Type != "supergroup" || m.Username != "osut" {
		t.Errorf("group identity not recorded: %+v", m)
	}
	if m.FirstName != "" {
		t.Errorf("group meta must not carry a speaker first_name, got %q", m.FirstName)
	}
	// A different speaker next: meta still names the group, unchanged.
	if err := s.Append(-100, TranscriptRecord{MsgID: 2, TS: fixedTS(2026, 7, 4, 10), Role: "user", Text: "yo", User: 8, Name: "Nick"},
		&ChatIdentity{Type: "supergroup", Title: "ОСУТ чат", Username: "osut"}); err != nil {
		t.Fatal(err)
	}
	if m = readMeta(filepath.Join(root, "-100")); m.FirstName != "" || m.Title != "ОСУТ чат" {
		t.Errorf("group meta drifted to a speaker: %+v", m)
	}
}

func TestTranscriptDayFileNaming(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	if err := s.Append(1, TranscriptRecord{MsgID: 1, TS: fixedTS(2026, 7, 3, 23), Role: "user", Text: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(1, TranscriptRecord{MsgID: 2, TS: fixedTS(2026, 7, 4, 1), Role: "user", Text: "b"}, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "1"))
	var days []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			days = append(days, e.Name())
		}
	}
	if len(days) != 2 {
		t.Fatalf("want 2 day-files, got %v", days)
	}
	// ReadDir returns sorted names; lexicographic order must equal chronological.
	if days[0] != "2026-07-03.jsonl" || days[1] != "2026-07-04.jsonl" {
		t.Errorf("day-files not chronological: %v", days)
	}
}

func TestTranscriptCompactJSONLGrepAnchor(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	// Two ids where one is a numeric prefix of the other: 5123 vs 51234.
	for _, id := range []int64{5123, 51234} {
		if err := s.Append(7, TranscriptRecord{MsgID: id, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, filepath.Join(root, "7", "2026-07-04.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	for _, l := range lines {
		if strings.Contains(l, ": ") {
			t.Errorf("line is not compact (has %q): %s", ": ", l)
		}
		if !strings.HasPrefix(l, `{"msg_id":`) {
			t.Errorf("line does not start with msg_id: %s", l)
		}
	}

	// The naive substring matches BOTH lines (5123 is a prefix of 51234) — this is
	// exactly the false positive the anchor guards against.
	naive := 0
	for _, l := range lines {
		if strings.Contains(l, `"msg_id":5123`) {
			naive++
		}
	}
	if naive != 2 {
		t.Errorf("expected naive substring to false-match both lines, matched %d", naive)
	}
	// The anchored form `"msg_id":5123[,}]` matches ONLY the real 5123 line.
	anchored := regexp.MustCompile(`"msg_id":5123[,}]`)
	hits := 0
	for _, l := range lines {
		if anchored.MatchString(l) {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("anchored regex should match exactly one line, matched %d", hits)
	}
}

func TestTranscriptRoleCounts(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	first := fixedTS(2026, 7, 4, 9)
	last := fixedTS(2026, 7, 4, 10)
	if err := s.Append(3, TranscriptRecord{MsgID: 1, TS: first, Role: "user", Text: "q"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(3, TranscriptRecord{MsgID: 2, TS: last, Role: "bot", Text: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	var m transcriptMeta
	b, _ := os.ReadFile(filepath.Join(root, "3", "meta.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.UserCount != 1 || m.BotCount != 1 {
		t.Errorf("counts wrong: user=%d bot=%d", m.UserCount, m.BotCount)
	}
	if !m.FirstSeen.Equal(first) || !m.LastSeen.Equal(last) {
		t.Errorf("seen times wrong: first=%v last=%v", m.FirstSeen, m.LastSeen)
	}
}

func TestTranscriptConcurrentAppends(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	const n = 40
	ts := fixedTS(2026, 7, 4, 9)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := s.Append(9, TranscriptRecord{MsgID: id, TS: ts, Role: "user", Text: "x"}, nil); err != nil {
				t.Errorf("append: %v", err)
			}
		}(int64(i))
	}
	wg.Wait()
	lines := readLines(t, filepath.Join(root, "9", "2026-07-04.jsonl"))
	if len(lines) != n {
		t.Errorf("want %d lines under concurrency, got %d", n, len(lines))
	}
	var m transcriptMeta
	b, _ := os.ReadFile(filepath.Join(root, "9", "meta.json"))
	json.Unmarshal(b, &m)
	if m.UserCount != n {
		t.Errorf("want user_count=%d, got %d", n, m.UserCount)
	}
}

func TestTranscriptTimestampSecondsRFC3339(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	// A sub-second timestamp must be truncated to whole seconds on write.
	ts := fixedTS(2026, 7, 4, 9).Add(500*time.Millisecond + 123*time.Nanosecond)
	if err := s.Append(1, TranscriptRecord{MsgID: 1, TS: ts, Role: "user", Text: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	line := readLines(t, filepath.Join(root, "1", "2026-07-04.jsonl"))[0]
	var raw struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.TS, ".") {
		t.Errorf("ts should have no fractional seconds: %q", raw.TS)
	}
	if _, err := time.Parse(time.RFC3339, raw.TS); err != nil {
		t.Errorf("ts is not RFC3339: %q (%v)", raw.TS, err)
	}
}

func TestTranscriptAuthorFields(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	// A group turn carries first_name in Name and the @handle in Username, kept apart.
	if err := s.Append(8, TranscriptRecord{
		MsgID: 1, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "hi",
		User: 777, Name: "Anton", Username: "akovalenko",
	}, nil); err != nil {
		t.Fatal(err)
	}
	// A speaker with no @handle: Username is omitted, Name still carries the first name.
	if err := s.Append(8, TranscriptRecord{
		MsgID: 2, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "hey",
		User: 888, Name: "Nick",
	}, nil); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(root, "8", "2026-07-04.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}

	var withHandle TranscriptRecord
	if err := json.Unmarshal([]byte(lines[0]), &withHandle); err != nil {
		t.Fatal(err)
	}
	if withHandle.Name != "Anton" || withHandle.Username != "akovalenko" || withHandle.User != 777 {
		t.Errorf("author fields not split: %+v", withHandle)
	}
	// The raw line keeps the msg_id grep anchor at the front despite the new fields.
	if !strings.HasPrefix(lines[0], `{"msg_id":1,`) {
		t.Errorf("author fields broke the grep anchor: %s", lines[0])
	}
	// A missing @handle is omitted from the JSON (private-side shape preserved).
	if strings.Contains(lines[1], "username") {
		t.Errorf("empty username should be omitted: %s", lines[1])
	}
}

func TestTranscriptReplyToOmittedWhenZero(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	if err := s.Append(5, TranscriptRecord{MsgID: 1, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "no reply"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(5, TranscriptRecord{MsgID: 2, TS: fixedTS(2026, 7, 4, 9), Role: "bot", ReplyTo: 1, Text: "reply"}, nil); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(root, "5", "2026-07-04.jsonl"))
	if strings.Contains(lines[0], "reply_to") {
		t.Errorf("zero reply_to should be omitted: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"reply_to":1`) {
		t.Errorf("set reply_to should be present: %s", lines[1])
	}
}
