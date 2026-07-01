package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// resultVersion is the on-disk schema version for a delivery Result. Bump only
// on a change older readers cannot tolerate.
const resultVersion = 1

// resultsSubdir is the outbox subdirectory holding delivery result descriptors,
// one per sent descriptor, correlated 1:1 by the descriptor's basename. It sits
// INSIDE the outbox so it is covered by the invocation's sandbox read/write
// capability and swept by the per-invocation RemoveAll on teardown (no separate
// reaper). Being a subdirectory it is invisible to the descriptor scan (which
// skips dirs), and — the outbox-root fsnotify watch being non-recursive —
// writing here never self-triggers a drain pass.
const resultsSubdir = "results"

// Result is the dispatcher's report on the delivery of one descriptor, written
// to <outbox>/results/<basename> on a TERMINAL outcome (success, permanent
// reject, or give-up) — never on a transient failure still to be retried. A
// blocking `send` polls for it to turn a fire-and-forget drop into synchronous
// feedback (see waitForResult).
type Result struct {
	V         int    `json:"v"`
	OK        bool   `json:"ok"`
	MessageID int64  `json:"message_id,omitempty"` // set on OK (reply-resurrection track)
	Error     string `json:"error,omitempty"`      // Telegram description / error text on !OK
	Permanent bool   `json:"permanent,omitempty"`  // true = responder must fix; false+!OK = give-up after retries
}

// writeResult atomically writes res as <outboxDir>/results/<base>. The temp file
// is created INSIDE results/ (not the outbox root), so its rename does not wake
// the root watcher, and the rename gives a reader an all-or-nothing view.
func writeResult(outboxDir, base string, res Result) error {
	if res.V == 0 {
		res.V = resultVersion
	}
	dir := filepath.Join(outboxDir, resultsSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating results dir: %w", err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshaling result: %w", err)
	}
	final := filepath.Join(dir, base)
	tmp := filepath.Join(dir, "."+base+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("writing result: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publishing result: %w", err)
	}
	return nil
}

// resultPollInterval is how often a waiting send re-checks for its result file.
const resultPollInterval = 50 * time.Millisecond

// resultWaitTimeout bounds how long send blocks on a delivery result before
// degrading to fire-and-forget. It is a hardcoded constant, not a flag: send
// takes no delivery options (the responder must not be burdened with them). 5s
// comfortably covers the ~1-RTT permanent-reject path this feature exists for,
// and stays under the client's 60s HTTP timeout.
const resultWaitTimeout = 5 * time.Second

// readResult loads and decodes one result file.
func readResult(path string) (*Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("malformed result: %w", err)
	}
	return &res, nil
}

// waitForResult polls <outboxDir>/results/<base> until the dispatcher publishes
// a delivery result or timeout elapses, returning (result, true) on a result or
// (nil, false) on timeout. Polling (not fsnotify) is deliberate: send is
// short-lived, the result lands within ~1 RTT, and polling sidesteps the
// watch-registered-after-create race with no catch-up logic. A not-yet-present
// (or momentarily unreadable) file just keeps polling.
func waitForResult(outboxDir, base string, timeout time.Duration) (*Result, bool) {
	path := filepath.Join(outboxDir, resultsSubdir, base)
	deadline := time.Now().Add(timeout)
	for {
		if res, err := readResult(path); err == nil {
			return res, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(resultPollInterval)
	}
}
