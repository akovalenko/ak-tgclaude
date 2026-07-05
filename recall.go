package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runRecall implements `ak-tgclaude recall --dir SCOPE <selector>`: a read-only,
// GROOMED view of the transcript store for the responder to consult. It exists so
// the model does not have to Read + grep the escaped JSONL itself — it asks for a
// message (and its thread neighbours) or a date range, and gets human-readable
// blocks back.
//
// SCOPE is the responder's own read scope ($AK_TGCLAUDE_TRANSCRIPT_DIR): a single
// chat directory, or — for the owner — the whole transcripts root. The shape is
// detected from the directory; the sandbox already confines SCOPE to what this
// responder may read (root deny-read + a per-invocation allowRead carve), so recall
// inherits that boundary without re-checking it. Read-only: it never writes.
func runRecall(args []string) error {
	req, err := parseRecallArgs(args)
	if err != nil {
		return err
	}
	return runRecallTo(os.Stdout, req)
}

// recallMode is the selector kind: a point lookup by message id, or a date-range
// dump. Exactly one is chosen at parse time.
type recallMode int

const (
	modeMsg recallMode = iota + 1
	modeRange
)

// recallReq is a parsed, validated recall invocation.
type recallReq struct {
	dir  string
	mode recallMode

	// modeMsg
	msg     int64
	context int // K records before/after the hit (non-piece), 0 = just the hit

	// modeRange
	since     time.Time // inclusive, local midnight
	until     time.Time // inclusive, local midnight (distantFuture when open)
	untilOpen bool      // --since with no --until: everything from since onward
	role      string    // "" = both, else "user"|"bot"
}

// distantFuture stands in for an open-ended --until so the range filter needs no
// special case. Well past any real Telegram timestamp.
var distantFuture = time.Date(9999, 12, 31, 23, 59, 59, 0, time.Local)

// parseRecallArgs parses and cross-validates the flags. Flag/selector mistakes come
// back as usageError (exit 2); everything data-dependent is decided later.
func parseRecallArgs(args []string) (recallReq, error) {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	dir := fs.String("dir", "", "transcript scope directory (a single chat, or the whole root)")
	msg := fs.Int64("msg", 0, "point lookup by message id (single-chat scope only)")
	context := fs.Int("context", 0, "with --msg: also show K records before and after the hit")
	day := fs.String("day", "", "dump one day (YYYY-MM-DD)")
	since := fs.String("since", "", "dump from this day onward (YYYY-MM-DD)")
	until := fs.String("until", "", "with --since: dump up to this day, inclusive (default: open)")
	role := fs.String("role", "", "with a range: keep only this role (user|bot)")
	if err := fs.Parse(args); err != nil {
		return recallReq{}, usageError{err}
	}

	req := recallReq{dir: *dir, msg: *msg, context: *context, role: *role}
	if *dir == "" {
		return req, usageError{errors.New("--dir is required (the transcript scope)")}
	}

	hasMsg := *msg != 0
	rangeGiven := *day != "" || *since != "" || *until != ""
	switch {
	case *msg < 0:
		return req, usageError{errors.New("--msg must be a positive message id")}
	case hasMsg && rangeGiven:
		return req, usageError{errors.New("choose either --msg or a day/range (--day/--since), not both")}
	case !hasMsg && !rangeGiven:
		return req, usageError{errors.New("need a selector: --msg N, --day DATE, or --since DATE [--until DATE]")}
	}

	if hasMsg {
		req.mode = modeMsg
		if *context < 0 {
			return req, usageError{errors.New("--context must be >= 0")}
		}
		if *role != "" {
			return req, usageError{errors.New("--role applies to a day/range dump, not --msg")}
		}
		return req, nil
	}

	// Range mode.
	req.mode = modeRange
	if *context != 0 {
		return req, usageError{errors.New("--context applies to --msg, not a day/range")}
	}
	if *role != "" && *role != "user" && *role != "bot" {
		return req, usageError{fmt.Errorf("--role must be user or bot, got %q", *role)}
	}
	if *day != "" {
		if *since != "" || *until != "" {
			return req, usageError{errors.New("--day cannot be combined with --since/--until")}
		}
		d, err := parseRecallDate("--day", *day)
		if err != nil {
			return req, err
		}
		req.since, req.until = d, d
		return req, nil
	}
	if *since == "" {
		return req, usageError{errors.New("--until needs --since")}
	}
	s, err := parseRecallDate("--since", *since)
	if err != nil {
		return req, err
	}
	req.since = s
	if *until == "" {
		req.until, req.untilOpen = distantFuture, true
		return req, nil
	}
	u, err := parseRecallDate("--until", *until)
	if err != nil {
		return req, err
	}
	if u.Before(s) {
		return req, usageError{errors.New("--until is before --since")}
	}
	req.until = u
	return req, nil
}

// parseRecallDate parses a YYYY-MM-DD flag value in the host's local zone (matching
// how day-files are named). A bad value is a usage error, named by its flag.
func parseRecallDate(flag, v string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", v, time.Local)
	if err != nil {
		return time.Time{}, usageError{fmt.Errorf("%s: %q is not a YYYY-MM-DD date", flag, v)}
	}
	return t, nil
}

// scopeShape is what --dir turned out to be.
type scopeShape int

const (
	shapeEmpty  scopeShape = iota // no history at all
	shapeSingle                   // one chat: day-files + meta.json directly under --dir
	shapeRoot                     // the whole root: one numbered subdir per chat
)

// chatRef is one chat to read: its id (the subdir name, = the Telegram chat id) and
// its directory. For a single-chat scope there is one, rooted at --dir itself.
type chatRef struct {
	id   string
	path string
}

// runRecallTo dispatches a parsed request against the detected scope shape, writing
// groomed output to w. Data-dependent errors (an unreadable dir, a point lookup in a
// multi-chat scope) are returned; an empty result is not an error.
func runRecallTo(w io.Writer, req recallReq) error {
	shape, chats, err := detectShape(req.dir)
	if err != nil {
		return fmt.Errorf("reading transcript scope %s: %w", req.dir, err)
	}
	if shape == shapeEmpty {
		fmt.Fprintln(w, "(no transcript history yet)")
		return nil
	}
	switch req.mode {
	case modeMsg:
		if shape != shapeSingle {
			return errors.New("point lookup needs a single-chat scope: a message id is not unique across chats")
		}
		return recallMsg(w, chats[0], req)
	default:
		return recallRange(w, shape, chats, req)
	}
}

// detectShape classifies --dir. Day-files or a meta.json directly under it => a
// single chat. Otherwise, subdirectories that themselves look like chat dirs => the
// whole root (sorted by name for a stable dump). Neither => empty.
func detectShape(dir string) (scopeShape, []chatRef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return shapeEmpty, nil, err
	}
	hasFile := false
	var subdirs []chatRef
	for _, e := range entries {
		if e.IsDir() {
			p := filepath.Join(dir, e.Name())
			if isChatDir(p) {
				subdirs = append(subdirs, chatRef{id: e.Name(), path: p})
			}
			continue
		}
		if e.Name() == "meta.json" || strings.HasSuffix(e.Name(), ".jsonl") {
			hasFile = true
		}
	}
	if hasFile {
		return shapeSingle, []chatRef{{id: filepath.Base(dir), path: dir}}, nil
	}
	if len(subdirs) > 0 {
		sort.Slice(subdirs, func(i, j int) bool { return subdirs[i].id < subdirs[j].id })
		return shapeRoot, subdirs, nil
	}
	return shapeEmpty, nil, nil
}

// isChatDir reports whether path holds a chat's transcript (a meta.json or any
// day-file). Best-effort: an unreadable dir is simply not a chat dir.
func isChatDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Name() == "meta.json" || strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// recallMsg renders a point lookup and its optional context window. A hit that is a
// split-message piece resolves to its anchor (the record that carries the text),
// with a note that the lookup arrived via a piece.
func recallMsg(w io.Writer, chat chatRef, req recallReq) error {
	recs, err := loadChatRecords(chat.path, nil)
	if err != nil {
		return err
	}
	idx := indexByMsgID(recs, req.msg)
	if idx < 0 {
		fmt.Fprintf(w, "(no message %d in this chat)\n", req.msg)
		return nil
	}

	target := idx
	if recs[idx].PartOf != 0 {
		piece, anchor := recs[idx].MsgID, recs[idx].PartOf
		aIdx := indexByMsgID(recs, anchor)
		if aIdx < 0 {
			fmt.Fprintf(w, "(msg %d is a piece of a split reply, but its anchor %d is missing)\n\n", piece, anchor)
			renderRecord(w, recs[idx])
			return nil
		}
		target = aIdx
		fmt.Fprintf(w, "(msg %d is a piece of a split reply — showing its anchor %d)\n\n", piece, anchor)
	}

	for _, i := range contextIndices(recs, target, -req.context) {
		renderRecord(w, recs[i])
	}
	renderRecord(w, recs[target])
	for _, i := range contextIndices(recs, target, req.context) {
		renderRecord(w, recs[i])
	}
	return nil
}

// recallRange dumps every non-piece record in [since,until], optionally filtered to
// one role. In the root shape it walks each chat, prefixing the block with a header
// naming the chat from its meta.json; chats with nothing in range are skipped.
func recallRange(w io.Writer, shape scopeShape, chats []chatRef, req recallReq) error {
	filter := func(d time.Time) bool { return !d.Before(req.since) && !d.After(req.until) }
	any := false
	for _, chat := range chats {
		recs, err := loadChatRecords(chat.path, filter)
		if err != nil {
			return err
		}
		var show []TranscriptRecord
		for _, r := range recs {
			if r.PartOf != 0 { // a split piece: its text lives on the anchor
				continue
			}
			if req.role != "" && r.Role != req.role {
				continue
			}
			show = append(show, r)
		}
		if len(show) == 0 {
			continue
		}
		if shape == shapeRoot {
			renderChatHeader(w, chat.id, readMeta(chat.path))
		}
		for _, r := range show {
			renderRecord(w, r)
		}
		any = true
	}
	if !any {
		fmt.Fprintf(w, "(no records for %s)\n", rangeDesc(req))
		return nil
	}
	return nil
}

// indexByMsgID returns the index of the record with msg id n, or -1.
func indexByMsgID(recs []TranscriptRecord, n int64) int {
	for i := range recs {
		if recs[i].MsgID == n {
			return i
		}
	}
	return -1
}

// contextIndices collects up to |k| non-piece record indices adjacent to target:
// k<0 walks backward, k>0 forward. The result is always in ascending (chronological)
// order. Split pieces are skipped — they carry no text.
func contextIndices(recs []TranscriptRecord, target, k int) []int {
	if k == 0 {
		return nil
	}
	var out []int
	if k < 0 {
		for i := target - 1; i >= 0 && len(out) < -k; i-- {
			if recs[i].PartOf == 0 {
				out = append(out, i)
			}
		}
		for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
			out[l], out[r] = out[r], out[l]
		}
		return out
	}
	for i := target + 1; i < len(recs) && len(out) < k; i++ {
		if recs[i].PartOf == 0 {
			out = append(out, i)
		}
	}
	return out
}

// loadChatRecords reads a chat's records in chronological order. filter, when
// non-nil, keeps only day-files whose date passes it (a record's date equals its
// file's date, so filtering by filename is exact). Malformed lines are skipped —
// the store only ever writes valid JSON, so a bad line means external tampering.
func loadChatRecords(chatDir string, filter func(time.Time) bool) ([]TranscriptRecord, error) {
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		d, ok := parseDayFileName(e.Name())
		if !ok || (filter != nil && !filter(d)) {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files) // ISO day names sort chronologically
	var recs []TranscriptRecord
	for _, name := range files {
		rs, err := readDayFile(filepath.Join(chatDir, name))
		if err != nil {
			return nil, err
		}
		recs = append(recs, rs...)
	}
	return recs, nil
}

// parseDayFileName extracts the local civil date from a YYYY-MM-DD.jsonl name.
func parseDayFileName(name string) (time.Time, bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	t, err := time.ParseInLocation("2006-01-02", base, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// readDayFile parses one day-file into records, in file (append) order.
func readDayFile(path string) ([]TranscriptRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []TranscriptRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a turn's text can be several KB
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec TranscriptRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue // skip a malformed line rather than abort the whole read
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return recs, nil
}

// readMeta loads a chat's meta.json, or nil if absent/unreadable.
func readMeta(chatDir string) *transcriptMeta {
	b, err := os.ReadFile(filepath.Join(chatDir, "meta.json"))
	if err != nil {
		return nil
	}
	var m transcriptMeta
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

// renderRecord writes one groomed block: a header line, the text with its real
// newlines, and one [attach: name] line per file the turn carried. A trailing blank
// line separates blocks.
func renderRecord(w io.Writer, rec TranscriptRecord) {
	fmt.Fprintln(w, groomHeader(rec))
	if rec.Text != "" {
		fmt.Fprintln(w, rec.Text)
	}
	for _, a := range rec.Attach {
		fmt.Fprintf(w, "[attach: %s]\n", a.Name)
	}
	fmt.Fprintln(w)
}

// groomHeader renders a record's header, e.g. "── [248] bot ↩246  2026-07-05
// 00:00:28 ──". The id/role/reply cluster is single-spaced; the author (groups
// only) and the timestamp are set off by double spaces.
func groomHeader(rec TranscriptRecord) string {
	cluster := fmt.Sprintf("[%d] %s", rec.MsgID, rec.Role)
	if rec.ReplyTo != 0 {
		cluster += fmt.Sprintf(" ↩%d", rec.ReplyTo)
	}
	segs := []string{cluster}
	if a := groomAuthor(rec); a != "" {
		segs = append(segs, a)
	}
	segs = append(segs, rec.TS.Local().Format("2006-01-02 15:04:05"))
	return "── " + strings.Join(segs, "  ") + " ──"
}

// groomAuthor renders a group turn's author as "Name (@handle)" (either part may be
// absent). Empty on the private side, where the record carries no author and the
// single partner is implied. A record with only a numeric user falls back to that id.
func groomAuthor(rec TranscriptRecord) string {
	switch {
	case rec.Name != "" && rec.Username != "":
		return rec.Name + " (@" + rec.Username + ")"
	case rec.Name != "":
		return rec.Name
	case rec.Username != "":
		return "@" + rec.Username
	case rec.User != 0:
		return fmt.Sprintf("user %d", rec.User)
	default:
		return ""
	}
}

// renderChatHeader prefixes a chat's block in the root shape, naming it from
// meta.json when available.
func renderChatHeader(w io.Writer, id string, m *transcriptMeta) {
	if who := metaWho(m); who != "" {
		fmt.Fprintf(w, "════════ chat %s — %s ════════\n\n", id, who)
		return
	}
	fmt.Fprintf(w, "════════ chat %s ════════\n\n", id)
}

// metaWho renders who a chat is from its meta.json, "Name (@handle)" style. A group
// is named by its title (the group has no single person); a private chat by its
// partner. "" when nothing is known.
func metaWho(m *transcriptMeta) string {
	if m == nil {
		return ""
	}
	switch {
	case m.Title != "" && m.Username != "":
		return m.Title + " (@" + m.Username + ")"
	case m.Title != "":
		return m.Title
	case m.FirstName != "" && m.Username != "":
		return m.FirstName + " (@" + m.Username + ")"
	case m.FirstName != "":
		return m.FirstName
	case m.Username != "":
		return "@" + m.Username
	default:
		return ""
	}
}

// rangeDesc describes the requested range for an empty-result note.
func rangeDesc(req recallReq) string {
	s := req.since.Format("2006-01-02")
	switch {
	case req.untilOpen:
		return "since " + s
	case req.until.Equal(req.since):
		return s
	default:
		return s + ".." + req.until.Format("2006-01-02")
	}
}
