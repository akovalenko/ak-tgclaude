package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UsageLog is an optional, append-only JSONL record of per-round resource use:
// one line per answered round carrying its wall-clock elapsed time and dollar
// cost. It is enabled SOLELY by configuring a path (usage_log / --usage-log) —
// there is no separate on/off flag; unset path => nil UsageLog => nothing is
// written and no disk is touched.
//
// The dispatcher is the only writer, but per-chat workers run concurrently, so a
// mutex guards the append. (O_APPEND is atomic on its own, but a full JSON line
// can exceed the pipe-atomic size, so we keep the write under the lock.)
type UsageLog struct {
	path string
	mu   sync.Mutex
}

// usageRecord is one round. The id fields (chat/user/msg) group together, then the
// metrics. MsgID is the incoming message's Telegram id, so a row joins the
// transcript store (keyed on chat_id + msg_id) for that turn. Cost is always
// emitted (0 when absent or zero), so it carries no omitempty.
type usageRecord struct {
	// TS marshals to RFC3339 in the host's local zone (offset included), truncated
	// to whole seconds — the same shape the transcript store uses, so a DB ingests
	// it as timestamptz. It is the round's START instant; TS + Elapsed = completion.
	TS      time.Time `json:"ts"`
	ChatID  int64     `json:"chat_id"`
	UserID  int64     `json:"user_id"` // 0 when the sender is unknown (e.g. channel posts)
	MsgID   int64     `json:"msg_id"`  // incoming message id; joins the transcript store
	Elapsed int64     `json:"elapsed"` // whole seconds (rounded), whole ROUND incl. any re-prompt
	Cost    float64   `json:"cost"`    // USD for the whole round (summed); 0 when absent
}

// NewUsageLog returns a log writing to path, creating its parent directory (the
// file itself is created lazily on the first Append). Callers pass "" to mean
// "feature off" and must not call this — a nil *UsageLog is the off state.
func NewUsageLog(path string) (*UsageLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("usage_log dir: %w", err)
	}
	return &UsageLog{path: path}, nil
}

// Append writes one round's record as a compact JSONL line. ts is truncated to
// whole seconds; elapsed is rounded to the nearest second; a negative cost is
// clamped to 0 (total_cost_usd is never negative, but be defensive). A nil
// receiver is a no-op, so a call site need not branch on the feature being on.
func (l *UsageLog) Append(ts time.Time, chatID, userID, msgID int64, elapsed time.Duration, cost float64) error {
	if l == nil {
		return nil
	}
	if cost < 0 {
		cost = 0
	}
	rec := usageRecord{
		TS:      ts.Truncate(time.Second),
		ChatID:  chatID,
		UserID:  userID,
		MsgID:   msgID,
		Elapsed: int64(elapsed.Round(time.Second) / time.Second),
		Cost:    cost,
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("marshal usage record: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("append %s: %w", l.path, werr)
	}
	if cerr != nil {
		return fmt.Errorf("close %s: %w", l.path, cerr)
	}
	return nil
}
