package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// readUsageRows reads a usage-log JSONL file into records (shared with the
// dispatch-level tests). A missing file yields nil, not an error, so an
// "off => nothing written" assertion reads naturally.
func readUsageRows(t *testing.T, path string) []usageRecord {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open usage log %s: %v", path, err)
	}
	defer f.Close()
	var rows []usageRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r usageRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("parse usage line %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan usage log: %v", err)
	}
	return rows
}

func TestUsageLogAppendWritesRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, err := NewUsageLog(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 7, 5, 9, 14, 7, 123_000_000, time.Local) // sub-second nanos to drop
	if err := l.Append(ts, 42, 5, 99, 3200*time.Millisecond, 0.5); err != nil {
		t.Fatal(err)
	}
	rows := readUsageRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.ChatID != 42 || r.UserID != 5 || r.MsgID != 99 {
		t.Errorf("ids = chat %d user %d msg %d, want 42/5/99", r.ChatID, r.UserID, r.MsgID)
	}
	if r.Elapsed != 3 { // 3.2s rounds to 3
		t.Errorf("elapsed = %d, want 3", r.Elapsed)
	}
	if r.Cost != 0.5 {
		t.Errorf("cost = %v, want 0.5", r.Cost)
	}
	if !r.TS.Equal(ts.Truncate(time.Second)) || r.TS.Nanosecond() != 0 {
		t.Errorf("ts = %v, want %v truncated to the second", r.TS, ts)
	}
}

func TestUsageLogRoundsElapsed(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{400 * time.Millisecond, 0},
		{600 * time.Millisecond, 1},
		{1400 * time.Millisecond, 1},
		{1600 * time.Millisecond, 2},
		{90 * time.Second, 90},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "u.jsonl")
		l, err := NewUsageLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Append(time.Now(), 1, 1, 1, tc.in, 0); err != nil {
			t.Fatal(err)
		}
		rows := readUsageRows(t, path)
		if len(rows) != 1 || rows[0].Elapsed != tc.want {
			t.Errorf("elapsed(%v) = %v, want %d", tc.in, rows, tc.want)
		}
	}
}

func TestUsageLogZeroCostWrittenAsZero(t *testing.T) {
	// Anton's rule: cost absent/zero => write 0 (present, not omitted).
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, err := NewUsageLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(time.Now(), 1, 1, 1, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cost":0`) {
		t.Errorf("zero cost should be written as \"cost\":0, got: %s", raw)
	}
}

func TestUsageLogClampsNegativeCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, err := NewUsageLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(time.Now(), 1, 1, 1, time.Second, -5); err != nil {
		t.Fatal(err)
	}
	rows := readUsageRows(t, path)
	if len(rows) != 1 || rows[0].Cost != 0 {
		t.Errorf("negative cost should clamp to 0, got %v", rows)
	}
}

func TestUsageLogConcurrentAppends(t *testing.T) {
	// Per-chat workers append concurrently; every line must land whole and parseable.
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, err := NewUsageLog(path)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Append(time.Now(), int64(i), int64(i), int64(i), time.Second, 0.001); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	rows := readUsageRows(t, path) // parses every line — a torn write would fail here
	if len(rows) != n {
		t.Fatalf("want %d rows, got %d", n, len(rows))
	}
}

func TestUsageLogCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "usage.jsonl")
	l, err := NewUsageLog(path)
	if err != nil {
		t.Fatalf("NewUsageLog should create the parent dir: %v", err)
	}
	if err := l.Append(time.Now(), 1, 1, 1, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if rows := readUsageRows(t, path); len(rows) != 1 {
		t.Fatalf("want 1 row in the created dir, got %d", len(rows))
	}
}

func TestUsageLogNilReceiverNoop(t *testing.T) {
	var l *UsageLog // the "feature off" state
	if err := l.Append(time.Now(), 1, 1, 1, time.Second, 1); err != nil {
		t.Errorf("nil UsageLog.Append should be a no-op, got %v", err)
	}
}
