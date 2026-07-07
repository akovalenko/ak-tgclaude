package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
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
func (u *uploader) deliver(ctx context.Context, d *Descriptor, r Route, s Sender, f *os.File, size int64) (int64, error) {
	// The uploader is an operator script run UNSANDBOXED, and both the source path
	// (arg1) and the destination name (arg2) carry a model-chosen basename — a
	// naive script that splices them into a shell command or an ssh/scp/rsync remote
	// path (which the far side re-parses) would let a name like file`rm -rf`.txt run
	// commands. We can't fix every uploader, so we refuse a dangerous name at the
	// gate and tell the model to rename. Fail-fast, not sanitize: the responder owns
	// the outbox and can just pick a saner name (spaces and non-ASCII are fine — see
	// uploadNameOK). This is enforced only on the upload path; a small file sent as a
	// Telegram attachment goes through argv/multipart, no shell.
	for _, n := range []string{d.Filename, filepath.Base(d.Path)} {
		if n != "" && !uploadNameOK(n) {
			return 0, &uploadError{fmt.Sprintf("Please choose a sane file name: %q has characters that aren't allowed for an uploaded file. "+
				"Use letters, digits, spaces, and . _ - , + @ %% (no quotes, backticks, $, or other shell metacharacters).", n)}
		}
	}
	if u.hardCapBytes > 0 && size > u.hardCapBytes {
		return 0, &uploadError{fmt.Sprintf("файл слишком большой даже для облака (%s > %s)", mbStr(size), mbStr(u.hardCapBytes))}
	}
	name := d.Filename
	if name == "" {
		name = filepath.Base(d.Path)
	}
	url, err := u.run(ctx, f, suggestName(name))
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

// uploadNameSafePunct is the punctuation allowed in an uploaded file name on top
// of Unicode letters and digits. Space is included (the uploader is expected to
// quote it — the shipped example uses rsync --protect-args); the shell-dangerous
// characters (quotes, backtick, $, ;, &, |, <, >, (), *, ?, …) are all absent, so
// an allowed name is safe to splice into a shell command or an ssh remote path.
const uploadNameSafePunct = " ._-,+@%"

// uploadNameOK reports whether name is safe to hand to the operator uploader
// (see deliver). Allowlist rather than denylist — bulletproof against a
// metacharacter we forgot: a name passes only if every rune is a Unicode letter,
// a digit, or one of uploadNameSafePunct. So "Отчёт за июль.pdf" passes and
// "file`rm -rf`.txt" does not. Empty is treated as OK (the caller skips it and
// falls back to another name).
func uploadNameOK(name string) bool {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(uploadNameSafePunct, r) {
			continue
		}
		return false
	}
	return true
}

// run executes the uploader on the already-open, symlink-vetted file (passing the
// suggested destination name as arg2) and returns the URL — the first non-blank
// line of stdout. A non-zero exit or empty output is an *uploadError carrying the
// stderr tail.
//
// The operator command is an external program run UNSANDBOXED that would re-open a
// path argument itself — and follow a symlink a responder planted in its writable
// outbox straight to a host secret. So instead of a path we hand it the fd we
// already opened with O_NOFOLLOW: ExtraFiles[0] becomes the child's fd 3 (0,1,2 are
// stdio) with close-on-exec cleared, and arg1 is /proc/self/fd/3 — a re-open of THAT
// resolves the exact inode we vetted, race-free, whatever the path now points to.
// The uploader contract already separates the source (arg1) from the destination
// name (arg2), so arg1 being a read handle rather than a real filename is fine.
const uploaderSourceFd = "/proc/self/fd/3"

func (u *uploader) run(ctx context.Context, file *os.File, name string) (string, error) {
	cmd := exec.CommandContext(ctx, u.command, uploaderSourceFd, name)
	cmd.ExtraFiles = []*os.File{file}
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
