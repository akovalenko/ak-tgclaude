package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// outboxEnv names the directory into which the responder drops outbound
// descriptors. The dispatcher sets it when spawning the responder and binds
// that directory to the invocation's route, so the responder never chooses a
// chat: it only writes its message, the dispatcher decides where it goes.
const outboxEnv = "AK_TGCLAUDE_OUTBOX"

const sendUsage = `ak-tgclaude send — enqueue an outbound Telegram message (run inside the responder sandbox)

usage: ak-tgclaude send <kind> [flags] [body]

kinds:
  text  [--html] [--silent] [--file F] [body|-]   a text message (plain, or Telegram HTML with --html)
  code  [--lang L] [--caption C] [--silent] [--file F] [body|-]
                                                  a preformatted block, optionally tagged with a language
  doc   [--filename N] [--caption C] [--silent] <path>
                                                  a file attachment

The body is --file F, the positional argument, or stdin ("-"/omitted). Prefer
--file for arbitrary text: it keeps message content (quotes, '!', …) out of argv.
The outbox directory comes from $AK_TGCLAUDE_OUTBOX (override with --outbox).
No route (chat/reply) is specified here; the dispatcher pins it in-process.
`

// runSend builds one descriptor from the sub-kind and flags, then drops it into
// the outbox spool.
func runSend(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, sendUsage)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]

	var (
		d      *Descriptor
		outbox string
		err    error
	)
	switch sub {
	case "text":
		d, outbox, err = parseSendText(rest)
	case "code":
		d, outbox, err = parseSendCode(rest)
	case "doc", "document":
		d, outbox, err = parseSendDoc(rest)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, sendUsage)
		return
	default:
		err = fmt.Errorf("unknown send kind %q", sub)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: send: %v\n\n%s", err, sendUsage)
		os.Exit(2)
	}

	dir, err := resolveOutbox(outbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: send: %v\n", err)
		os.Exit(1)
	}
	path, err := d.Drop(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: send: %v\n", err)
		os.Exit(1)
	}
	// Block on the dispatcher's delivery result so the responder sees a Telegram
	// rejection (bad HTML, blocked chat, …) as a non-zero exit and can fix and
	// resend in the same turn. There are deliberately no flags to skip or retune
	// this — the responder is not to be burdened with delivery options.
	base := filepath.Base(path)
	res, ok := waitForResult(dir, base, resultWaitTimeout)
	if ok {
		// Cosmetic: retire our own result file promptly (teardown's RemoveAll
		// would sweep it regardless); the write is covered by the outbox grant.
		_ = os.Remove(filepath.Join(dir, resultsSubdir, base))
	}
	os.Exit(reportResult(os.Stderr, res, !ok))
}

// reportResult maps a delivery outcome to an exit code, writing any diagnostic
// to w. A timeout degrades to fire-and-forget (exit 0; the drop stays queued and
// drain finishes in the background). A successful delivery is silent (exit 0). A
// permanent reject prints the Telegram error plus a fix-and-resend hint, and a
// give-up prints the failure — both exit non-zero so the responder's shell sees
// the send fail. res is nil exactly when timedOut is true.
func reportResult(w io.Writer, res *Result, timedOut bool) int {
	// Writes to w (stderr) are diagnostics; a failed write cannot change the
	// outcome, so the error is deliberately discarded.
	switch {
	case timedOut:
		_, _ = fmt.Fprintf(w, "ak-tgclaude: send: queued; delivery outcome unknown after %s\n", resultWaitTimeout)
		return 0
	case res.OK:
		return 0
	case res.Permanent:
		_, _ = fmt.Fprintf(w, "ak-tgclaude: send: Telegram rejected the message: %s\n", res.Error)
		_, _ = fmt.Fprintln(w, "ak-tgclaude: send: fix the HTML and resend.")
		return 1
	default:
		_, _ = fmt.Fprintf(w, "ak-tgclaude: send: delivery failed after retries: %s\n", res.Error)
		return 1
	}
}

func parseSendText(args []string) (*Descriptor, string, error) {
	fs := flag.NewFlagSet("send text", flag.ContinueOnError)
	html := fs.Bool("html", false, "treat the body as Telegram HTML (parse_mode=HTML)")
	silent := fs.Bool("silent", false, "send without a notification")
	file := fs.String("file", "", "read the body from this file (keeps message text out of argv)")
	outbox := fs.String("outbox", "", "outbox dir (default: $AK_TGCLAUDE_OUTBOX)")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	text, err := resolveBody(*file, fs.Args())
	if err != nil {
		return nil, "", err
	}
	d := &Descriptor{Kind: KindText, Text: text, Silent: *silent}
	if *html {
		d.Format = FormatHTML
	}
	return d, *outbox, nil
}

func parseSendCode(args []string) (*Descriptor, string, error) {
	fs := flag.NewFlagSet("send code", flag.ContinueOnError)
	lang := fs.String("lang", "", "source language tag (e.g. go, python)")
	caption := fs.String("caption", "", "optional line shown before the block")
	silent := fs.Bool("silent", false, "send without a notification")
	file := fs.String("file", "", "read the body from this file (keeps message text out of argv)")
	outbox := fs.String("outbox", "", "outbox dir (default: $AK_TGCLAUDE_OUTBOX)")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	code, err := resolveBody(*file, fs.Args())
	if err != nil {
		return nil, "", err
	}
	d := &Descriptor{Kind: KindCode, Code: code, Language: *lang, Caption: *caption, Silent: *silent}
	return d, *outbox, nil
}

func parseSendDoc(args []string) (*Descriptor, string, error) {
	fs := flag.NewFlagSet("send doc", flag.ContinueOnError)
	filename := fs.String("filename", "", "displayed file name (default: basename of path)")
	caption := fs.String("caption", "", "optional caption")
	silent := fs.Bool("silent", false, "send without a notification")
	outbox := fs.String("outbox", "", "outbox dir (default: $AK_TGCLAUDE_OUTBOX)")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return nil, "", fmt.Errorf("expected exactly one file path, got %d", len(pos))
	}
	path, err := filepath.Abs(pos[0])
	if err != nil {
		return nil, "", fmt.Errorf("resolving %s: %w", pos[0], err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("attachment %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("attachment %s is a directory", path)
	}
	d := &Descriptor{Kind: KindDocument, Path: path, Filename: *filename, Caption: *caption, Silent: *silent}
	return d, *outbox, nil
}

// resolveBody returns the message body from --file if set, else from the
// positional argument/stdin. --file is the responder's path: it writes the body
// with the Write tool and passes only the filename here, so message text (with
// shell metacharacters like '!') never reaches the command line.
func resolveBody(file string, pos []string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading body file: %w", err)
		}
		return string(b), nil
	}
	return bodyArg(pos)
}

// bodyArg returns the message body: the single positional argument, or stdin
// when the argument is "-" or omitted (so large bodies can be piped in).
func bodyArg(pos []string) (string, error) {
	if len(pos) == 0 || (len(pos) == 1 && pos[0] == "-") {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return string(b), nil
	}
	if len(pos) == 1 {
		return pos[0], nil
	}
	return "", fmt.Errorf("expected one body argument or '-' for stdin, got %d", len(pos))
}

// resolveOutbox picks the outbox directory (flag override, else env) and checks
// that it exists and is a directory.
func resolveOutbox(override string) (string, error) {
	dir := override
	if dir == "" {
		dir = os.Getenv(outboxEnv)
	}
	if dir == "" {
		return "", fmt.Errorf("no outbox directory: set %s or pass --outbox", outboxEnv)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("outbox %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("outbox %s is not a directory", dir)
	}
	return dir, nil
}
