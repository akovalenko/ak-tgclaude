package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Retry/back-off policy for transient send failures. A permanent reject (a 4xx
// the responder must fix) is quarantined immediately; a transient one (429 /
// 5xx / network) is retried with exponential back-off from baseBackoff, capped
// at maxBackoff, and quarantined as a give-up after maxSendAttempts. Per-
// descriptor attempt counts live in the attempts map serveOutbox threads
// through each pass — in-memory only, bounded by the invocation's lifetime (the
// outbox is RemoveAll'd on teardown, so cross-invocation durable retry is out
// of scope).
const (
	maxSendAttempts = 6
	baseBackoff     = 2 * time.Second
	maxBackoff      = 1 * time.Minute
)

// classify decides whether a send error is permanent (quarantine now) or
// transient (retry with back-off), and surfaces a 429's authoritative
// retry_after. A non-APIError (network / timeout / decode) is transient.
func classify(err error) (permanent bool, retryAfter time.Duration) {
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Code == 429:
			return false, time.Duration(ae.RetryAfter) * time.Second
		case ae.Code >= 500:
			return false, 0
		case ae.Code >= 400:
			return true, 0 // 400/403/404: bad request, blocked, chat-not-found — retry won't help
		}
		return false, 0 // any other non-OK: be lenient, treat as transient
	}
	return false, 0
}

// backoff is exponential from baseBackoff, doubling per attempt, capped at
// maxBackoff.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := baseBackoff << (attempt - 1)
	if d <= 0 || d > maxBackoff { // <=0 guards the shift overflowing at a high attempt
		d = maxBackoff
	}
	return d
}

// arm (re)schedules t to fire after d, draining any pending tick first so a
// stale fire cannot trigger a spurious extra pass. d <= 0 leaves t stopped.
func arm(t *time.Timer, d time.Duration) {
	if d <= 0 {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// serveOutbox delivers descriptors from one invocation's outbox to Telegram,
// in drop order, for the lifetime of that responder. The directory is bound to
// one Route (the dispatcher pins chat/reply per invocation).
//
// It is the SOLE drainer of dir, so there is never a concurrent send/remove
// race: it drains what is already present (catch-up — the responder may write
// before the watcher registers), streams new drops via fsnotify, and on close
// of stop does a final flush so nothing the responder left is lost. On parent
// ctx cancellation (shutdown) it returns without flushing; the spool is durable
// and undelivered descriptors are picked up on the next run.
func serveOutbox(ctx context.Context, dir string, r Route, s Sender, stop <-chan struct{}) {
	var events chan fsnotify.Event
	var errs chan error
	if w, err := fsnotify.NewWatcher(); err != nil {
		log.Printf("ak-tgclaude: watch %s: %v (falling back to flush-on-stop)", dir, err)
	} else {
		defer w.Close()
		if err := w.Add(dir); err != nil {
			log.Printf("ak-tgclaude: watch %s: %v", dir, err)
		} else {
			events, errs = w.Events, w.Errors
		}
	}

	// attempts carries per-descriptor transient-retry counts across passes; retry
	// fires a re-drain once a transient back-off elapses (created stopped).
	attempts := map[string]int{}
	retry := time.NewTimer(0)
	if !retry.Stop() {
		<-retry.C
	}
	defer retry.Stop()

	drain := func() {
		retryIn, err := drainExisting(ctx, dir, r, s, attempts)
		if err != nil {
			log.Printf("ak-tgclaude: drain %s: %v", dir, err)
			return
		}
		arm(retry, retryIn)
	}

	drain()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			// Responder finished: flush with a fresh, bounded context (the
			// parent ctx may still be live, but we want a definite deadline).
			flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, err := drainExisting(flushCtx, dir, r, s, attempts); err != nil {
				log.Printf("ak-tgclaude: final flush %s: %v", dir, err)
			}
			cancel()
			return
		case <-retry.C:
			drain()
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename) == 0 || !isDescriptor(ev.Name) {
				continue
			}
			drain()
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			log.Printf("ak-tgclaude: watch %s: %v", dir, err)
		}
	}
}

// isDescriptor reports whether name is a published descriptor: a .json file
// that is not a hidden temp file.
func isDescriptor(name string) bool {
	base := filepath.Base(name)
	return strings.HasSuffix(base, ".json") && !strings.HasPrefix(base, ".")
}

// drainExisting sends every descriptor currently in dir, in name (drop) order.
// A successful send removes the descriptor and clears its retry state. An
// unparseable descriptor is quarantined in dir/bad/ and skipped. A permanent
// reject (a 4xx the responder must fix) is quarantined and the pass CONTINUES,
// so one bad descriptor can never wedge the queue behind it. A transient
// failure (429 / 5xx / network) stops the pass — leaving that descriptor and
// everything after it in place (head-of-line, preserving order) — and returns
// the back-off after which the caller should retry; after maxSendAttempts the
// descriptor is quarantined as a give-up and the pass continues. A returned
// retryIn of 0 means nothing is waiting on a retry timer.
func drainExisting(ctx context.Context, dir string, r Route, s Sender, attempts map[string]int) (retryIn time.Duration, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && isDescriptor(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		path := filepath.Join(dir, name)
		d, err := readDescriptor(path)
		if err != nil {
			quarantine(dir, path, err)
			continue
		}
		if _, sendErr := sendDescriptor(ctx, d, r, s); sendErr != nil {
			permanent, retryAfter := classify(sendErr)
			if permanent {
				quarantine(dir, path, sendErr)
				delete(attempts, name)
				continue
			}
			attempts[name]++
			if attempts[name] >= maxSendAttempts {
				quarantine(dir, path, fmt.Errorf("gave up after %d attempts: %w", attempts[name], sendErr))
				delete(attempts, name)
				continue
			}
			if retryAfter <= 0 {
				retryAfter = backoff(attempts[name])
			}
			return retryAfter, nil // stop: preserve order, retry after back-off
		}
		delete(attempts, name)
		if err := os.Remove(path); err != nil {
			return 0, fmt.Errorf("removing sent %s: %w", name, err)
		}
	}
	return 0, nil
}

// readDescriptor loads and validates one descriptor file.
func readDescriptor(path string) (*Descriptor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Descriptor
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("malformed descriptor: %w", err)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// sendDescriptor renders one descriptor and delivers it, spilling an oversized
// text/code message to a document.
func sendDescriptor(ctx context.Context, d *Descriptor, r Route, s Sender) (int64, error) {
	if d.Kind == KindDocument {
		return s.SendDocument(ctx, r, d.Path, d.Filename, d.Caption, "", d.Silent)
	}
	text, mode := renderMessage(d)
	if fits(text) {
		return s.SendMessage(ctx, r, text, mode, d.Silent)
	}
	tmp, err := os.CreateTemp("", "ak-tgclaude-spill-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(spillPayload(d)); err != nil {
		tmp.Close()
		return 0, err
	}
	tmp.Close()
	return s.SendDocument(ctx, r, tmp.Name(), spillName(d), d.Caption, "", d.Silent)
}

// quarantine moves an unsendable descriptor out of the active spool so it does
// not block the queue forever.
func quarantine(dir, path string, cause error) {
	bad := filepath.Join(dir, "bad")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		log.Printf("ak-tgclaude: quarantine %s: %v", filepath.Base(path), err)
		return
	}
	if err := os.Rename(path, filepath.Join(bad, filepath.Base(path))); err != nil {
		log.Printf("ak-tgclaude: quarantine %s: %v", filepath.Base(path), err)
		return
	}
	log.Printf("ak-tgclaude: quarantined %s: %v", filepath.Base(path), cause)
}
