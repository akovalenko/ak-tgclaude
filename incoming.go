package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Attachment is an incoming file the dispatcher saved into the responder's
// outbox. It is surfaced to the model in the prompt (path + description) so the
// model can read or Edit it in place, and send it back with send_document.
type Attachment struct {
	Path     string // absolute path under <outbox>/incoming/
	Filename string // sanitized display name (as the user named it)
	MimeType string // Telegram's declared MIME type ("" if unknown)
	Size     int64  // bytes actually written
}

// describe renders the human-facing "name, size, mime" line for the prompt.
func (a *Attachment) describe() string {
	parts := []string{a.Filename}
	if a.Size > 0 {
		parts = append(parts, humanBytes(a.Size))
	}
	if a.MimeType != "" {
		parts = append(parts, a.MimeType)
	}
	return strings.Join(parts, ", ")
}

// incomingSubdir is the outbox subdirectory incoming attachments land in, kept
// separate from the files the responder itself authors (and from $TMPDIR
// scratch) so the two never collide.
const incomingSubdir = "incoming"

// fetchIncomingDocument resolves the message's document, downloads it under
// <docDir>/incoming/<msgid>-<name>, and returns the saved Attachment. The size
// cap is enforced twice: the caller rejects a declared FileSize over the cap
// before calling this, and the download itself is bounded (cap+1) so a document
// whose declared size lied still cannot overrun the disk.
func (d *Dispatcher) fetchIncomingDocument(ctx context.Context, m *Message, docDir string) (*Attachment, error) {
	doc := m.Document
	filePath, err := d.client.GetFile(ctx, doc.FileID)
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", err)
	}

	dir := filepath.Join(docDir, incomingSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("incoming dir: %w", err)
	}
	name := sanitizeFilename(doc.FileName)
	if name == "" {
		// No usable name from the user; fall back to the server path's basename.
		name = sanitizeFilename(filepath.Base(filePath))
	}
	if name == "" {
		name = "file"
	}
	// The msgid prefix keeps re-sends of the same name from clobbering each other;
	// sanitizeFilename already stripped any directory components, so the join
	// cannot escape the incoming dir.
	dest := filepath.Join(dir, fmt.Sprintf("%d-%s", m.MessageID, name))

	f, err := os.Create(dest)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", dest, err)
	}
	limit := d.maxIncomingBytes
	copyLimit := int64(0)
	if limit > 0 {
		copyLimit = limit + 1 // one over the cap, so an over-limit read is detectable
	}
	written, derr := d.client.DownloadFile(ctx, filePath, f, copyLimit)
	cerr := f.Close()
	if derr != nil {
		os.Remove(dest)
		return nil, fmt.Errorf("download: %w", derr)
	}
	if cerr != nil {
		os.Remove(dest)
		return nil, fmt.Errorf("close %s: %w", dest, cerr)
	}
	if limit > 0 && written > limit {
		os.Remove(dest)
		return nil, fmt.Errorf("attachment exceeds the %d-byte cap", limit)
	}
	return &Attachment{Path: dest, Filename: name, MimeType: doc.MimeType, Size: written}, nil
}

// sanitizeFilename reduces an untrusted Telegram file name to a bare, safe
// basename: it strips any directory components (defeating path traversal), drops
// the "."/".." specials, and neutralizes stray separators. The result is only
// ever used as the tail of a <msgid>-<name> file inside the incoming dir.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.TrimSpace(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, `\`, "_")
	return name
}

// humanBytes renders a byte count as a compact human size (B/KB/MB/GB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
