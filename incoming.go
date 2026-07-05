package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
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

// incomingSpec is a message's incoming media reduced to a single downloadable
// file — an attached document, or the largest rendition of an attached photo —
// so the fetch/cap/sanitize path is written once for both.
type incomingSpec struct {
	FileID   string
	FileName string // "" => derive a name from the downloaded path
	MimeType string
	FileSize int64 // declared size, gated against the cap before the download
}

// incomingFile reduces a message's media to one downloadable spec (a document,
// or the largest size of a photo), or nil when it carries neither.
func incomingFile(m *Message) *incomingSpec {
	if d := m.Document; d != nil {
		return &incomingSpec{FileID: d.FileID, FileName: d.FileName, MimeType: d.MimeType, FileSize: d.FileSize}
	}
	if p := largestPhoto(m.Photo); p != nil {
		// Photos carry no name or MIME and are always JPEG — synthesize both.
		return &incomingSpec{FileID: p.FileID, FileName: "photo.jpg", MimeType: "image/jpeg", FileSize: p.FileSize}
	}
	return nil
}

// effectiveIncoming picks the file to fetch for a message: its own attachment if
// it has one, else — when the message replies to one that carried a file — the
// replied-to file (transcripts keep only metadata, so re-fetching the replied-to
// file_id is the only path back to its bytes). Returns the spec (nil if none),
// whether it came from the reply, and the message id to name the download after.
// The message's own file always wins over the replied-to one.
func effectiveIncoming(m *Message) (spec *incomingSpec, fromReply bool, srcMsgID int64) {
	if s := incomingFile(m); s != nil {
		return s, false, m.MessageID
	}
	if m.ReplyTo != nil {
		if s := incomingFile(m.ReplyTo); s != nil {
			return s, true, m.ReplyTo.MessageID
		}
	}
	return nil, false, m.MessageID
}

// fetchIncoming downloads the spec's file under <docDir>/incoming/<msgid>-<name>
// and returns the saved Attachment. The size cap is enforced twice: the caller
// rejects a declared FileSize over the cap before calling this, and the download
// itself is bounded (cap+1) so a file whose declared size lied still cannot
// overrun the disk.
func (d *Dispatcher) fetchIncoming(ctx context.Context, spec *incomingSpec, msgID int64, docDir string) (*Attachment, error) {
	filePath, err := d.client.GetFile(ctx, spec.FileID)
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", err)
	}

	dir := filepath.Join(docDir, incomingSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("incoming dir: %w", err)
	}
	name := sanitizeFilename(spec.FileName)
	if name == "" {
		// No usable name from the sender; fall back to the server path's basename.
		name = sanitizeFilename(filepath.Base(filePath))
	}
	if name == "" {
		name = "file"
	}
	// The msgid prefix keeps re-sends of the same name from clobbering each other;
	// sanitizeFilename already stripped any directory components, so the join
	// cannot escape the incoming dir.
	dest := filepath.Join(dir, fmt.Sprintf("%d-%s", msgID, name))

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
	return &Attachment{Path: dest, Filename: name, MimeType: stripControl(spec.MimeType), Size: written}, nil
}

// stripControl removes control characters and the Unicode line/paragraph
// separators from s. Names and MIME types arrive from outside (an incoming
// Telegram file's name/mime, or a model-chosen send name) and land in sinks that
// treat a newline as a delimiter: the prompt preamble (where a \n would break out
// of the announced-file line into the instruction zone) and the sendDocument
// multipart header (where a CRLF could inject a second form field — e.g. a
// chat_id that retargets the pinned route). Dropping these — while keeping
// spaces, punctuation, and non-ASCII — neutralizes both without rejecting a
// legitimate name.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		// unicode.IsControl covers C0/C1 (incl. \n, \r, \t, DEL); U+2028/U+2029 are the
		// Unicode line/paragraph separators (Zl/Zp, not control) a model could read as a
		// newline.
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
}

// sanitizeFilename reduces an untrusted Telegram file name to a bare, safe
// basename: it strips any directory components (defeating path traversal), the
// "."/".." specials, stray separators, and control characters (stripControl —
// so a \n cannot smuggle a line into the prompt preamble that announces the
// file). The result is only ever used as the tail of a <msgid>-<name> file
// inside the incoming dir.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.TrimSpace(name)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, `\`, "_")
	return stripControl(name)
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
