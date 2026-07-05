package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ap appends a record with a default timestamp when none is set.
func ap(t *testing.T, s *TranscriptStore, chat int64, rec TranscriptRecord, ident *ChatIdentity) {
	t.Helper()
	if rec.TS.IsZero() {
		rec.TS = fixedTS(2026, 7, 4, 9)
	}
	if err := s.Append(chat, rec, ident); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// singleChatScope builds a one-chat transcript and returns the chat's directory
// (the scope a normal user's responder is handed).
func singleChatScope(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	s := NewTranscriptStore(root)
	ap(t, s, 42, TranscriptRecord{MsgID: 100, Role: "user", Text: "how do I restart asmo?"}, nil)
	ap(t, s, 42, TranscriptRecord{MsgID: 101, Role: "bot", ReplyTo: 100, Text: "Run deploy restart asmo."}, nil)
	ap(t, s, 42, TranscriptRecord{MsgID: 102, Role: "user", Text: "thanks", Attach: []TranscriptAttach{{Kind: "document", Name: "log.txt"}}}, nil)
	// A split reply: anchor 200 carries the text; piece 201 is a stub.
	ap(t, s, 42, TranscriptRecord{MsgID: 200, Role: "bot", ReplyTo: 102, Text: "a very long answer"}, nil)
	ap(t, s, 42, TranscriptRecord{MsgID: 201, Role: "bot", PartOf: 200}, nil)
	return filepath.Join(root, "42")
}

func TestRecallShapeDetection(t *testing.T) {
	single := singleChatScope(t)
	if shape, chats, err := detectShape(single); err != nil || shape != shapeSingle || len(chats) != 1 {
		t.Fatalf("single: shape=%v chats=%d err=%v", shape, len(chats), err)
	}

	root := t.TempDir()
	s := NewTranscriptStore(root)
	ap(t, s, 42, TranscriptRecord{MsgID: 1, Role: "user", Text: "a"}, &ChatIdentity{FirstName: "Anton", Username: "ak"})
	ap(t, s, 77, TranscriptRecord{MsgID: 1, Role: "user", Text: "b"}, &ChatIdentity{FirstName: "Nick"})
	shape, chats, err := detectShape(root)
	if err != nil || shape != shapeRoot || len(chats) != 2 {
		t.Fatalf("root: shape=%v chats=%d err=%v", shape, len(chats), err)
	}
	if chats[0].id != "42" || chats[1].id != "77" { // sorted by name
		t.Errorf("root chats not sorted: %+v", chats)
	}

	if shape, _, _ := detectShape(t.TempDir()); shape != shapeEmpty {
		t.Errorf("empty dir should be shapeEmpty, got %v", shape)
	}
}

func TestRecallMsgPointLookup(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{dir: scope, mode: modeMsg, msg: 101}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "[101] bot ↩100") {
		t.Errorf("header missing id/role/reply: %q", out)
	}
	if !strings.Contains(out, "Run deploy restart asmo.") {
		t.Errorf("text missing: %q", out)
	}
	// Only the one record — no neighbours without --context.
	if strings.Contains(out, "how do I restart asmo?") {
		t.Errorf("point lookup leaked a neighbour: %q", out)
	}
}

func TestRecallMsgPieceResolvesToAnchor(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{dir: scope, mode: modeMsg, msg: 201}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "showing its anchor 200") {
		t.Errorf("missing piece->anchor note: %q", out)
	}
	if !strings.Contains(out, "a very long answer") {
		t.Errorf("anchor text not shown: %q", out)
	}
	if !strings.Contains(out, "[200] bot") {
		t.Errorf("anchor header not shown: %q", out)
	}
}

func TestRecallMsgNotFound(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{dir: scope, mode: modeMsg, msg: 999}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no message 999") {
		t.Errorf("expected not-found note, got %q", b.String())
	}
}

func TestRecallMsgInRootErrors(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	ap(t, s, 42, TranscriptRecord{MsgID: 1, Role: "user", Text: "a"}, nil)
	ap(t, s, 77, TranscriptRecord{MsgID: 1, Role: "user", Text: "b"}, nil)
	err := runRecallTo(&bytes.Buffer{}, recallReq{dir: root, mode: modeMsg, msg: 1})
	if err == nil || !strings.Contains(err.Error(), "single-chat scope") {
		t.Fatalf("point lookup in root should error about scope, got %v", err)
	}
}

func TestRecallContext(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	// Context 1 around msg 101: one non-piece record before (100) and after (102).
	if err := runRecallTo(&b, recallReq{dir: scope, mode: modeMsg, msg: 101, context: 1}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"how do I restart asmo?", "Run deploy restart asmo.", "thanks"} {
		if !strings.Contains(out, want) {
			t.Errorf("context window missing %q in %q", want, out)
		}
	}
	// Chronological order: 100 before 101 before 102.
	if i, j, k := strings.Index(out, "[100]"), strings.Index(out, "[101]"), strings.Index(out, "[102]"); !(i < j && j < k) {
		t.Errorf("context not in chronological order: 100@%d 101@%d 102@%d", i, j, k)
	}
}

func TestRecallRangeDaySkipsPiecesAndRendersAttach(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{
		dir: scope, mode: modeRange,
		since: mustDate(t, "2026-07-04"), until: mustDate(t, "2026-07-04"),
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The anchor is shown; the piece (empty stub) is not counted as its own block.
	if strings.Count(out, "── [201]") != 0 {
		t.Errorf("split piece should be skipped in a range dump: %q", out)
	}
	if !strings.Contains(out, "[attach: log.txt]") {
		t.Errorf("attachment note missing: %q", out)
	}
	// All four real turns present.
	for _, id := range []string{"[100]", "[101]", "[102]", "[200]"} {
		if !strings.Contains(out, id) {
			t.Errorf("range dump missing %s: %q", id, out)
		}
	}
}

func TestRecallRangeRoleFilter(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{
		dir: scope, mode: modeRange, role: "user",
		since: mustDate(t, "2026-07-04"), until: mustDate(t, "2026-07-04"),
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "[100] user") || !strings.Contains(out, "[102] user") {
		t.Errorf("user records missing under role filter: %q", out)
	}
	if strings.Contains(out, "bot") {
		t.Errorf("role=user must drop bot turns: %q", out)
	}
}

func TestRecallRangeSinceUntilWindow(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	ap(t, s, 42, TranscriptRecord{MsgID: 1, TS: fixedTS(2026, 7, 3, 9), Role: "user", Text: "before"}, nil)
	ap(t, s, 42, TranscriptRecord{MsgID: 2, TS: fixedTS(2026, 7, 4, 9), Role: "user", Text: "inside"}, nil)
	ap(t, s, 42, TranscriptRecord{MsgID: 3, TS: fixedTS(2026, 7, 6, 9), Role: "user", Text: "after"}, nil)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{
		dir: filepath.Join(root, "42"), mode: modeRange,
		since: mustDate(t, "2026-07-04"), until: mustDate(t, "2026-07-05"),
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "inside") || strings.Contains(out, "before") || strings.Contains(out, "after") {
		t.Errorf("since/until window wrong: %q", out)
	}
}

func TestRecallRootRangeChatHeaders(t *testing.T) {
	root := t.TempDir()
	s := NewTranscriptStore(root)
	ap(t, s, 42, TranscriptRecord{MsgID: 1, Role: "user", Text: "from anton"}, &ChatIdentity{FirstName: "Anton", Username: "ak"})
	ap(t, s, 77, TranscriptRecord{MsgID: 1, Role: "user", Text: "from nick"}, &ChatIdentity{FirstName: "Nick"})
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{
		dir: root, mode: modeRange,
		since: mustDate(t, "2026-07-04"), until: mustDate(t, "2026-07-04"),
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "chat 42 — Anton (@ak)") {
		t.Errorf("chat header with identity missing: %q", out)
	}
	if !strings.Contains(out, "chat 77 — Nick") {
		t.Errorf("chat header with bare first name missing: %q", out)
	}
	if !strings.Contains(out, "from anton") || !strings.Contains(out, "from nick") {
		t.Errorf("both chats' records should appear: %q", out)
	}
}

func TestRecallEmptyRange(t *testing.T) {
	scope := singleChatScope(t)
	var b bytes.Buffer
	if err := runRecallTo(&b, recallReq{
		dir: scope, mode: modeRange,
		since: mustDate(t, "2020-01-01"), until: mustDate(t, "2020-01-02"),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no records for 2020-01-01..2020-01-02") {
		t.Errorf("expected empty-range note, got %q", b.String())
	}
}

func TestGroomAuthor(t *testing.T) {
	cases := []struct {
		rec  TranscriptRecord
		want string
	}{
		{TranscriptRecord{Name: "Anton", Username: "ak"}, "Anton (@ak)"},
		{TranscriptRecord{Name: "Anton"}, "Anton"},
		{TranscriptRecord{Username: "ak"}, "@ak"},
		{TranscriptRecord{User: 555}, "user 555"},
		{TranscriptRecord{}, ""}, // private side: no author
	}
	for _, c := range cases {
		if got := groomAuthor(c.rec); got != c.want {
			t.Errorf("groomAuthor(%+v) = %q, want %q", c.rec, got, c.want)
		}
	}
}

func TestMetaWho(t *testing.T) {
	cases := []struct {
		m    *transcriptMeta
		want string
	}{
		{&transcriptMeta{Title: "ОСУТ", Username: "osut"}, "ОСУТ (@osut)"}, // public group: title + handle
		{&transcriptMeta{Title: "Team", Type: "group"}, "Team"},            // private group: title only
		{&transcriptMeta{FirstName: "Anton", Username: "ak"}, "Anton (@ak)"},
		{&transcriptMeta{FirstName: "Anton"}, "Anton"},
		{&transcriptMeta{Username: "ak"}, "@ak"},
		{nil, ""},
		{&transcriptMeta{}, ""},
	}
	for _, c := range cases {
		if got := metaWho(c.m); got != c.want {
			t.Errorf("metaWho(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestParseRecallArgsValidation(t *testing.T) {
	bad := [][]string{
		{"--msg", "5"},  // no --dir
		{"--dir", "/x"}, // no selector
		{"--dir", "/x", "--msg", "5", "--day", "2026-07-04"},              // two selectors
		{"--dir", "/x", "--msg", "5", "--role", "user"},                   // role with --msg
		{"--dir", "/x", "--day", "2026-07-04", "--context", "2"},          // context with range
		{"--dir", "/x", "--day", "2026-07-04", "--since", "2026-07-01"},   // day + since
		{"--dir", "/x", "--until", "2026-07-04"},                          // until without since
		{"--dir", "/x", "--since", "2026-07-05", "--until", "2026-07-01"}, // until before since
		{"--dir", "/x", "--day", "nope"},                                  // bad date
		{"--dir", "/x", "--day", "2026-07-04", "--role", "boss"},          // bad role
		{"--dir", "/x", "--msg", "-3"},                                    // negative id
	}
	for _, args := range bad {
		if _, err := parseRecallArgs(args); err == nil {
			t.Errorf("parseRecallArgs(%v) should have failed", args)
		}
	}

	// A valid --msg with context.
	req, err := parseRecallArgs([]string{"--dir", "/x", "--msg", "5", "--context", "2"})
	if err != nil || req.mode != modeMsg || req.msg != 5 || req.context != 2 {
		t.Fatalf("valid --msg parse wrong: %+v err=%v", req, err)
	}
	// A valid open-ended --since.
	req, err = parseRecallArgs([]string{"--dir", "/x", "--since", "2026-07-01"})
	if err != nil || req.mode != modeRange || !req.untilOpen {
		t.Fatalf("valid --since parse wrong: %+v err=%v", req, err)
	}
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		t.Fatalf("bad test date %q: %v", s, err)
	}
	return d
}
