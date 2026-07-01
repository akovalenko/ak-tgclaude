package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readResultFile reads and decodes <outbox>/results/<base>, failing the test if
// it is missing or malformed.
func readResultFile(t *testing.T, outbox, base string) Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(outbox, resultsSubdir, base))
	if err != nil {
		t.Fatalf("reading result %s: %v", base, err)
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("unmarshal result %s: %v", base, err)
	}
	return res
}

func TestWriteResultAtomicRoundTrip(t *testing.T) {
	outbox := t.TempDir()
	base := "00000000000000000001-1-00000000000000000001.json"
	if err := writeResult(outbox, base, Result{OK: true, MessageID: 42}); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	got := readResultFile(t, outbox, base)
	if !got.OK || got.MessageID != 42 || got.V != resultVersion {
		t.Errorf("round-trip = %+v, want OK/42/v%d", got, resultVersion)
	}
	// The atomic rename must leave no temp file behind in results/.
	ents, err := os.ReadDir(filepath.Join(outbox, resultsSubdir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Errorf("want exactly one result file, have %d: %v", len(ents), ents)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestDrainWritesResultOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path, err := (&Descriptor{Kind: KindText, Text: "hi"}).Drop(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	f := &fakeSender{}
	if _, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{}); err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	res := readResultFile(t, dir, base)
	if !res.OK || res.MessageID != 1 {
		t.Errorf("result = %+v, want OK with message_id 1", res)
	}
}

func TestDrainWritesResultOnPermanent(t *testing.T) {
	dir := t.TempDir()
	path, err := (&Descriptor{Kind: KindText, Text: "hi"}).Drop(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	f := &fakeSender{err: &APIError{Code: 400, Description: "Bad Request: can't parse entities"}}
	if _, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, map[string]int{}); err != nil {
		t.Fatalf("drainExisting: %v", err)
	}
	res := readResultFile(t, dir, base)
	if res.OK || !res.Permanent {
		t.Errorf("want !OK permanent, got %+v", res)
	}
	if !strings.Contains(res.Error, "can't parse entities") {
		t.Errorf("result error should carry the Telegram description, got %q", res.Error)
	}
	if bad := badDir(t, dir); len(bad) != 1 {
		t.Errorf("permanent reject should be quarantined, bad/=%v", bad)
	}
}

func TestDrainWritesResultOnGiveup(t *testing.T) {
	dir := t.TempDir()
	path, err := (&Descriptor{Kind: KindText, Text: "hi"}).Drop(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	f := &fakeSender{err: &APIError{Code: 500}} // always transient
	attempts := map[string]int{}
	for i := 0; i < maxSendAttempts; i++ {
		if _, err := drainExisting(context.Background(), dir, Route{ChatID: 1}, f, attempts); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	res := readResultFile(t, dir, base)
	if res.OK || res.Permanent {
		t.Errorf("give-up want !OK !permanent, got %+v", res)
	}
	if res.Error == "" {
		t.Errorf("give-up result should carry an error text")
	}
	if bad := badDir(t, dir); len(bad) != 1 {
		t.Errorf("given-up descriptor should be quarantined, bad/=%v", bad)
	}
}
