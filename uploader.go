package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// uploader is the dispatcher's large-file fallback for send_document. When a
// document exceeds thresholdBytes, the dispatcher (which runs UNSANDBOXED, unlike
// the responder) runs an operator command on the file and delivers the returned
// URL as a chat message instead of a Telegram attachment — Telegram's bot API caps
// an attachment near 50 MB. The command is invoked as argv [command, file, name]:
// it must print the public URL on stdout (first non-blank line) and exit 0, or exit
// non-zero with a message on stderr. name is a collision-free basename a smart
// uploader can use as its destination (see suggestName); a simple one ignores it.
type uploader struct {
	command        string
	thresholdBytes int64
	hardCapBytes   int64 // 0 = no hard cap (advertised max unset)
}

// newUploader builds the runtime uploader from config, or nil if the fallback is
// off (no command). The hard cap sits 10% above the advertised max, so a file a
// touch over the advertised number still uploads; a 0 max leaves no cap.
func newUploader(command string, thresholdMB, maxMB int) *uploader {
	if command == "" {
		return nil
	}
	var hardCap int64
	if maxMB > 0 {
		hardCap = (int64(maxMB) * 11 / 10) << 20
	}
	return &uploader{
		command:        command,
		thresholdBytes: int64(thresholdMB) << 20,
		hardCapBytes:   hardCap,
	}
}

// uploadError marks a failure of the upload path (too large, uploader crashed, no
// URL). callTool surfaces its message to the model verbatim, rather than under the
// "Telegram rejected" prefix used for genuine Telegram API errors.
type uploadError struct{ msg string }

func (e *uploadError) Error() string { return e.msg }

// deliver uploads the document and sends its URL on route r. size is the file size
// the caller already stat'd (and found over the threshold). A file over the hard
// cap is rejected before the uploader runs.
func (u *uploader) deliver(ctx context.Context, d *Descriptor, r Route, s Sender, size int64) (int64, error) {
	if u.hardCapBytes > 0 && size > u.hardCapBytes {
		return 0, &uploadError{fmt.Sprintf("файл слишком большой даже для облака (%s > %s)", mbStr(size), mbStr(u.hardCapBytes))}
	}
	name := d.Filename
	if name == "" {
		name = filepath.Base(d.Path)
	}
	url, err := u.run(ctx, d.Path, suggestName(name))
	if err != nil {
		return 0, err
	}
	text := url
	if name != "" {
		text = name + "\n" + url
	}
	if d.Caption != "" {
		text = d.Caption + "\n" + text
	}
	return s.SendMessage(ctx, r, text, "", d.Silent)
}

// run executes the uploader on file (passing the suggested destination name as
// arg2) and returns the URL — the first non-blank line of stdout. A non-zero exit
// or empty output is an *uploadError carrying the stderr tail.
func (u *uploader) run(ctx context.Context, file, name string) (string, error) {
	cmd := exec.CommandContext(ctx, u.command, file, name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &uploadError{"upload failed: " + msg}
	}
	url := firstNonBlankLine(stdout.String())
	if url == "" {
		return "", &uploadError{"upload failed: the uploader printed no URL"}
	}
	return url, nil
}

// suggestName returns a collision-free basename: a short random prefix joined to
// the original name (a3f9c2e1-dist.tar.gz), so two files that share a name (two
// dist.tar.gz) get distinct destinations on the share host. The original name is
// preserved whole (extension included) for recognizability. On the near-impossible
// rand failure it degrades to the plain name (best-effort collision avoidance).
func suggestName(orig string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return orig
	}
	return hex.EncodeToString(b[:]) + "-" + orig
}

// mbStr renders a byte count as whole megabytes (rounded up), for the size-limit
// error message.
func mbStr(n int64) string {
	return fmt.Sprintf("%d MB", (n+(1<<20)-1)/(1<<20))
}

// firstNonBlankLine returns the first line of s with non-blank content, trimmed —
// the uploader's URL, tolerant of a trailing newline or an incidental blank line.
func firstNonBlankLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
