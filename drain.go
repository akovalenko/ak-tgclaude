package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DrainOutbox delivers descriptors from a single outbox directory to Telegram,
// in drop order, until ctx is cancelled. The directory is bound to one Route:
// the dispatcher spawns a responder, gives it a private outbox, and runs a
// drain for it. (A future topology could resolve the route per descriptor; the
// seam is this Route parameter.)
//
// It first drains whatever is already present (catch-up after a restart — the
// spool is durable), then watches for new drops (fsnotify) and re-drains, with
// a periodic retry tick so a transient send failure eventually recovers.
func DrainOutbox(ctx context.Context, dir string, r Route, s Sender) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watching %s: %w", dir, err)
	}

	// Catch-up before watching, so drops that landed while we were down are sent.
	if err := drainExisting(ctx, dir, r, s); err != nil {
		log.Printf("ak-tgclaude: drain %s: %v", dir, err)
	}

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename) == 0 || !isDescriptor(ev.Name) {
				continue
			}
			if err := drainExisting(ctx, dir, r, s); err != nil {
				log.Printf("ak-tgclaude: drain %s: %v", dir, err)
			}
		case <-tick.C:
			if err := drainExisting(ctx, dir, r, s); err != nil {
				log.Printf("ak-tgclaude: drain %s: %v", dir, err)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
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

// drainExisting sends every descriptor currently in dir, in name (drop) order,
// removing each on success. An unparseable descriptor is quarantined in
// dir/bad/ and skipped; a transient send failure stops the pass — leaving the
// file and everything after it — so a retry preserves order (head-of-line).
func drainExisting(ctx context.Context, dir string, r Route, s Sender) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
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
			return err
		}
		path := filepath.Join(dir, name)
		d, err := readDescriptor(path)
		if err != nil {
			quarantine(dir, path, err)
			continue
		}
		if _, err := sendDescriptor(ctx, d, r, s); err != nil {
			return fmt.Errorf("sending %s: %w", name, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing sent %s: %w", name, err)
		}
	}
	return nil
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
