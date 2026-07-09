package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestMCPTokenRidesEnvNotFile is the file→env migration invariant: the capability
// token is carried in the parent's env (mcpTokenEnv), NOT written to a config file;
// the inline --mcp-config references it by env var, so the literal token never
// enters argv or the disk. This replaces the old 0600-file transport (M3), whose
// file sat in the shared /tmp under one UID and only denied its OWN reader — a
// parallel responder could read a sibling's token file and hijack its route. The
// env-ref removes that whole file-hole class; cross-instance reads of the value are
// blocked by the sandbox pid-ns (a sibling's Bash cannot reach this claude's
// /proc/<pid>/environ) plus the credentials.envVars scrub asserted in TestMaterialize.
func TestMCPTokenRidesEnvNotFile(t *testing.T) {
	c := &claudeResponder{project: "/proj", denyPaths: []string{"/cfg/bot.toml"}}
	req := RespondRequest{DocDir: "/o/x", MCPURL: "http://127.0.0.1:9/mcp", MCPToken: "seekrit"}

	// The token VALUE is in the parent's env, under mcpTokenEnv (the parent expands it
	// into the Authorization header; the model's sandboxed Bash has it scrubbed).
	var tokenEnv string
	for _, kv := range c.env(req) {
		if strings.HasPrefix(kv, mcpTokenEnv+"=") {
			tokenEnv = strings.TrimPrefix(kv, mcpTokenEnv+"=")
		}
	}
	if tokenEnv != "seekrit" {
		t.Errorf("%s = %q, want the token value in the parent env", mcpTokenEnv, tokenEnv)
	}

	// The inline config references the token by env var — never embeds the literal.
	cfg := buildMCPConfig(req.MCPURL, mcpTokenEnv)
	if !strings.Contains(cfg, "${"+mcpTokenEnv+"}") {
		t.Errorf("config should reference the token by env var, got: %s", cfg)
	}
	if strings.Contains(cfg, "seekrit") {
		t.Errorf("config must not embed the literal token: %s", cfg)
	}
	if !strings.Contains(cfg, "127.0.0.1:9") {
		t.Errorf("config should carry the MCP url: %s", cfg)
	}

	// Deny is just the absolute set — there is no per-invocation config file to deny.
	if strings.Join(c.filePolicy(req).Deny, ",") != "/cfg/bot.toml" {
		t.Errorf("Deny should be only the absolute set, got: %v", c.filePolicy(req).Deny)
	}
}

func TestFilePolicy(t *testing.T) {
	// The single-source mirror policy: writeRoots is the outbox; ReadAllow carves the
	// own outbox + own transcript scope; ReadDeny masks the shared roots (sibling
	// outboxes, other transcripts); Deny is the absolute set. A future writable dir
	// would be one more entry in writeRoots here and flow to both hook and sandbox.
	c := &claudeResponder{
		project:        "/proj",
		outboxRoot:     "/run/out",
		transcriptRoot: "/s/transcripts",
		denyPaths:      []string{"/host/.ssh", "/cfg/bot.toml"},
	}
	pol := c.filePolicy(RespondRequest{DocDir: "/run/out/o1", TranscriptScope: "/s/transcripts/42"})
	for _, tc := range []struct {
		name, got, want string
	}{
		{"writeRoots", strings.Join(pol.WriteRoots, ","), "/run/out/o1"},
		{"readAllow", strings.Join(pol.ReadAllow, ","), "/run/out/o1,/s/transcripts/42"},
		{"readDeny", strings.Join(pol.ReadDeny, ","), "/run/out,/s/transcripts"},
		{"deny", strings.Join(pol.Deny, ","), "/host/.ssh,/cfg/bot.toml"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// Non-owner (no usage-log env value) carves nothing for the usage log.
	if strings.Contains(strings.Join(pol.ReadAllow, ","), "usage") {
		t.Errorf("non-owner readAllow must not carry the usage log: %v", pol.ReadAllow)
	}
}

func TestEnvCarriesFilePolicy(t *testing.T) {
	// The dispatcher hands the whole policy to the hook as JSON in one env var.
	c := &claudeResponder{project: "/proj", denyPaths: []string{"/cfg/bot.toml"}}
	var got string
	for _, kv := range c.env(RespondRequest{DocDir: "/run/out/o1"}) {
		if strings.HasPrefix(kv, filePolicyEnv+"=") {
			got = strings.TrimPrefix(kv, filePolicyEnv+"=")
		}
	}
	if got == "" {
		t.Fatal("filePolicyEnv not set in the responder env")
	}
	var p hookFilePolicy
	if err := json.Unmarshal([]byte(got), &p); err != nil {
		t.Fatalf("policy env is not valid JSON: %v (%q)", err, got)
	}
	if strings.Join(p.WriteRoots, ",") != "/run/out/o1" || strings.Join(p.Deny, ",") != "/cfg/bot.toml" {
		t.Errorf("policy env round-trip wrong: %+v", p)
	}
}

func TestBuildArgs(t *testing.T) {
	base := "-p --output-format json --setting-sources project --permission-mode dontAsk"

	// No docDir and no MCP endpoint, no debug, no passthrough => bare args.
	if got := strings.Join((&claudeResponder{}).buildArgs(RespondRequest{}), " "); got != base {
		t.Errorf("bare args = %q", got)
	}

	// --debug (alone) is inserted right after the base flags when enabled.
	if got := strings.Join((&claudeResponder{debug: true}).buildArgs(RespondRequest{}), " "); got != base+" --debug" {
		t.Errorf("debug args = %q", got)
	}

	// Operator passthrough is appended verbatim, after everything else.
	if got := strings.Join((&claudeResponder{claudeArgs: []string{"--model", "opus", "--effort", "high"}}).buildArgs(RespondRequest{}), " "); got != base+" --model opus --effort high" {
		t.Errorf("passthrough args = %q", got)
	}

	got := (&claudeResponder{agent: "eputs-telegram-guide"}).buildArgs(RespondRequest{
		SessionID: "sess-7", DocDir: "/run/out/outbox-A1",
		MCPURL: "http://127.0.0.1:9/mcp", MCPToken: "tok9",
	})
	joined := strings.Join(got, " ")
	// MCP wiring: --mcp-config is inline JSON referencing the token by env var
	// (Bearer ${AK_TGCLAUDE_MCP_TOKEN}), strict-only, send tools permitted under dontAsk.
	if !strings.Contains(joined, "--mcp-config ") || !strings.Contains(joined, "--strict-mcp-config") {
		t.Errorf("expected inline --mcp-config: %q", joined)
	}
	if !strings.Contains(joined, "${"+mcpTokenEnv+"}") || !strings.Contains(joined, "127.0.0.1:9/mcp") {
		t.Errorf("inline config should carry the env-ref and the url: %q", joined)
	}
	// The LITERAL token must NOT appear in argv (that was the /proc/<pid>/cmdline leak —
	// the header now carries only the env-var NAME, expanded by the parent at runtime).
	if strings.Contains(joined, "tok9") {
		t.Errorf("literal token must not be in argv: %q", joined)
	}
	if !strings.Contains(joined, "--allowedTools mcp__tg__send_message,mcp__tg__send_code,mcp__tg__send_document") {
		t.Errorf("expected --allowedTools with the send tools: %q", joined)
	}
	// A --settings overlay scopes sandbox access to that outbox, before --agent/--resume.
	// There is no per-invocation config file now, so the overlay carries no denyRead.
	if !strings.Contains(joined, `"allowWrite":["/run/out/outbox-A1"]`) ||
		!strings.Contains(joined, `"allowRead":["/run/out/outbox-A1"]`) {
		t.Errorf("overlay missing per-invocation sandbox grants: %q", joined)
	}
	if strings.Contains(joined, `"denyRead"`) {
		t.Errorf("overlay should carry no denyRead (the mcp-config file is gone): %q", joined)
	}
	if !strings.HasSuffix(joined, "--agent eputs-telegram-guide --resume sess-7") {
		t.Errorf("agent/resume should come last: %q", joined)
	}
}

func TestBuildArgsAppendSystemPrompt(t *testing.T) {
	// On a FRESH spawn (empty sessionID), the persona is injected via
	// --append-system-prompt.
	fresh := strings.Join((&claudeResponder{agent: "faq-responder"}).buildArgs(RespondRequest{AppendSystemPrompt: "PERSONA TEXT"}), " ")
	if !strings.Contains(fresh, "--append-system-prompt PERSONA TEXT") {
		t.Errorf("fresh spawn should inject the persona: %q", fresh)
	}
	// On a RESUME the persona is frozen into the session, so it is NOT re-sent even
	// if passed.
	resume := strings.Join((&claudeResponder{agent: "faq-responder"}).buildArgs(RespondRequest{SessionID: "sess-1", AppendSystemPrompt: "PERSONA TEXT"}), " ")
	if strings.Contains(resume, "--append-system-prompt") {
		t.Errorf("resume should not re-send the persona: %q", resume)
	}
	// Empty persona => no flag.
	none := strings.Join((&claudeResponder{agent: "faq-responder"}).buildArgs(RespondRequest{}), " ")
	if strings.Contains(none, "--append-system-prompt") {
		t.Errorf("empty persona should add no flag: %q", none)
	}
}

func TestBuildArgsExtraTools(t *testing.T) {
	// Operator extra tools join --allowedTools after the send tools, deduped; a
	// duplicate of a send tool is not repeated.
	got := strings.Join((&claudeResponder{extraTools: []string{"Agent", "WebFetch", "mcp__tg__send_message"}}).
		buildArgs(RespondRequest{MCPURL: "http://127.0.0.1:9/mcp", MCPToken: "tok"}), " ")
	want := "--allowedTools mcp__tg__send_message,mcp__tg__send_code,mcp__tg__send_document,Skill,Agent,WebFetch"
	if !strings.Contains(got, want) {
		t.Errorf("extra tools not merged into --allowedTools\nwant substring: %q\ngot: %q", want, got)
	}
}

func TestBuildArgsScopedToolKeepsScope(t *testing.T) {
	// A scoped extra tool reaches --allowedTools VERBATIM (scope preserved, "*"
	// literal — args are exec.Command elements, never shell-expanded), and two scopes
	// of the same verb are BOTH kept as distinct permission rules — the opposite of
	// the frontmatter, which collapses them to one bare name.
	got := strings.Join((&claudeResponder{extraTools: []string{"WebFetch(domain:github.com)", "WebFetch(domain:*.github.com)"}}).
		buildArgs(RespondRequest{MCPURL: "http://127.0.0.1:9/mcp", MCPToken: "tok"}), " ")
	want := "--allowedTools mcp__tg__send_message,mcp__tg__send_code,mcp__tg__send_document,Skill,WebFetch(domain:github.com),WebFetch(domain:*.github.com)"
	if !strings.Contains(got, want) {
		t.Errorf("scoped tools not kept verbatim in --allowedTools\nwant substring: %q\ngot: %q", want, got)
	}
}

func TestBuildInvocationSettings(t *testing.T) {
	if buildInvocationSettings(nil, "", "", false) != "" {
		t.Errorf("empty write roots + empty scope => empty overlay")
	}
	s := buildInvocationSettings([]string{"/o/x"}, "", "", false)
	if !strings.Contains(s, `"allowWrite":["/o/x"]`) || !strings.Contains(s, `"allowRead":["/o/x"]`) {
		t.Errorf("overlay JSON wrong: %s", s)
	}
	// The Write TOOL grant is the hook's job now — the overlay is sandbox-only.
	if strings.Contains(s, "permissions") || strings.Contains(s, "Write(") {
		t.Errorf("overlay should carry no permissions/Write, got: %s", s)
	}
	// Must NOT touch sandbox.enabled etc. (would clobber the merged base).
	if strings.Contains(s, "enabled") {
		t.Errorf("overlay should only carry filesystem allow*, got: %s", s)
	}
}

func TestBuildInvocationSettingsTranscriptScope(t *testing.T) {
	s := buildInvocationSettings([]string{"/o/x"}, "/s/transcripts/42", "", false)
	if !strings.Contains(s, `"allowWrite":["/o/x"]`) {
		t.Errorf("allowWrite should be the outbox only: %s", s)
	}
	if !strings.Contains(s, `"allowRead":["/o/x","/s/transcripts/42"]`) {
		t.Errorf("allowRead should include outbox + transcript scope: %s", s)
	}
	// Scope-only (no outbox) still grants read, and carries no allowWrite.
	s2 := buildInvocationSettings(nil, "/s/transcripts", "", false)
	if !strings.Contains(s2, `"allowRead":["/s/transcripts"]`) || strings.Contains(s2, "allowWrite") {
		t.Errorf("scope-only overlay wrong: %s", s2)
	}
}

func TestBuildArgsThreadsScope(t *testing.T) {
	joined := strings.Join((&claudeResponder{}).buildArgs(RespondRequest{DocDir: "/o/x", TranscriptScope: "/s/transcripts/42"}), " ")
	if !strings.Contains(joined, `"allowRead":["/o/x","/s/transcripts/42"]`) {
		t.Errorf("transcript scope should reach the --settings allowRead: %q", joined)
	}
}

func TestBuildInvocationSettingsUsageLog(t *testing.T) {
	// Owner: the usage-log file is carved into allowRead (alongside the outbox), never
	// denyRead — the owner may grep/awk it.
	owner := buildInvocationSettings([]string{"/o/x"}, "", "/v/usage.jsonl", true)
	if !strings.Contains(owner, `"allowRead":["/o/x","/v/usage.jsonl"]`) {
		t.Errorf("owner overlay should allowRead the usage log: %s", owner)
	}
	if strings.Contains(owner, "denyRead") {
		t.Errorf("owner overlay must not denyRead: %s", owner)
	}
	// Non-owner: the usage-log file is denied — the whole point (read is otherwise
	// default-open). It never appears in allowRead.
	other := buildInvocationSettings([]string{"/o/x"}, "", "/v/usage.jsonl", false)
	if !strings.Contains(other, `"denyRead":["/v/usage.jsonl"]`) {
		t.Errorf("non-owner overlay should denyRead the usage log: %s", other)
	}
	if strings.Contains(other, `"allowRead":["/o/x","/v/usage.jsonl"]`) {
		t.Errorf("non-owner overlay must not allowRead the usage log: %s", other)
	}
	// Feature off (empty usage path): neither allow nor deny of any usage file.
	off := buildInvocationSettings([]string{"/o/x"}, "", "", false)
	if strings.Contains(off, "denyRead") || strings.Contains(off, "usage") {
		t.Errorf("no usage path => no allow/deny for it: %s", off)
	}
}

func TestBuildArgsThreadsUsageLog(t *testing.T) {
	// Owner path reaches the --settings allowRead.
	owner := strings.Join((&claudeResponder{}).buildArgs(RespondRequest{DocDir: "/o/x", UsageLogPath: "/v/usage.jsonl", UsageLogOwner: true}), " ")
	if !strings.Contains(owner, `"allowRead":["/o/x","/v/usage.jsonl"]`) {
		t.Errorf("owner usage log should reach --settings allowRead: %q", owner)
	}
	// Non-owner path reaches the --settings denyRead.
	other := strings.Join((&claudeResponder{}).buildArgs(RespondRequest{DocDir: "/o/x", UsageLogPath: "/v/usage.jsonl"}), " ")
	if !strings.Contains(other, `"denyRead":["/v/usage.jsonl"]`) {
		t.Errorf("non-owner usage log should reach --settings denyRead: %q", other)
	}
}

func TestUsageLogEnvValue(t *testing.T) {
	// The env var / prompt hint carry the path ONLY for the owner — a non-owner is told
	// nothing (and is denied the file), even though UsageLogPath is set for them.
	if v := (RespondRequest{UsageLogPath: "/v/u.jsonl", UsageLogOwner: true}).usageLogEnvValue(); v != "/v/u.jsonl" {
		t.Errorf("owner env value = %q, want the path", v)
	}
	if v := (RespondRequest{UsageLogPath: "/v/u.jsonl", UsageLogOwner: false}).usageLogEnvValue(); v != "" {
		t.Errorf("non-owner env value = %q, want empty", v)
	}
}

func TestBuildPromptUsageLog(t *testing.T) {
	// Owner (usageLog non-empty): the prompt announces the file + points at tg-usage.
	p := buildPrompt("/code", RespondRequest{DocDir: "/out", UsageLogPath: "/v/usage.jsonl", UsageLogOwner: true, Prompt: "how much did we spend?"})
	if !strings.Contains(p, "/v/usage.jsonl") || !strings.Contains(p, "tg-usage") {
		t.Errorf("owner prompt should announce the usage log + tg-usage: %q", p)
	}
	// Non-owner (empty): no usage-log line at all.
	q := buildPrompt("/code", RespondRequest{DocDir: "/out", Prompt: "hi"})
	if strings.Contains(q, "Usage log") || strings.Contains(q, "tg-usage") {
		t.Errorf("non-owner prompt must not mention the usage log: %q", q)
	}
}

func TestMergeNoProxy(t *testing.T) {
	// Loopback is always present so the MCP request bypasses any host proxy.
	got := mergeNoProxy("", "")
	for _, h := range []string{"127.0.0.1", "localhost", "::1"} {
		if !strings.Contains(got, h) {
			t.Errorf("mergeNoProxy should include %s: %q", h, got)
		}
	}
	// Existing entries are preserved and everything is de-duplicated.
	got = mergeNoProxy("example.com, 127.0.0.1", "localhost")
	if !strings.Contains(got, "example.com") {
		t.Errorf("should preserve existing entries: %q", got)
	}
	if n := strings.Count(got, "127.0.0.1"); n != 1 {
		t.Errorf("127.0.0.1 should appear once, got %d: %q", n, got)
	}
	if n := strings.Count(got, "localhost"); n != 1 {
		t.Errorf("localhost should appear once, got %d: %q", n, got)
	}
}

func TestBuildPrompt(t *testing.T) {
	sent := time.Date(2026, 7, 3, 14, 5, 0, 0, time.UTC)
	p := buildPrompt("/home/bot/code", RespondRequest{DocDir: "/run/out/outbox-A1", Prompt: "how does foo work?", SentAt: sent})
	if !strings.Contains(p, "Project directory (read-only): /home/bot/code") {
		t.Errorf("missing literal project path: %q", p)
	}
	if !strings.Contains(p, "Outbox directory (write attachment/scratch files here): /run/out/outbox-A1") {
		t.Errorf("missing literal outbox path: %q", p)
	}
	if !strings.Contains(p, "not shell-expanded") {
		t.Errorf("missing the tool-vs-shell caveat: %q", p)
	}
	if !strings.Contains(p, "PERSISTS across replies") {
		t.Errorf("missing the outbox-persistence hint: %q", p)
	}
	// The send time is stamped into the message header (rendered in its own zone).
	if !strings.Contains(p, "Incoming Telegram message (sent 2026-07-03 14:05 UTC) to answer:") {
		t.Errorf("missing/malformed send-time stamp: %q", p)
	}
	// The untrusted message is appended last, verbatim.
	if !strings.HasSuffix(p, "how does foo work?") {
		t.Errorf("message should be appended last: %q", p)
	}
}

// A zero SentAt omits the stamp entirely (no 1970 epoch leaking into the prompt).
func TestBuildPromptOmitsZeroTime(t *testing.T) {
	p := buildPrompt("/p", RespondRequest{DocDir: "/o", Prompt: "hi"})
	if !strings.Contains(p, "Incoming Telegram message to answer:") {
		t.Errorf("zero time should yield the unstamped header: %q", p)
	}
	if strings.Contains(p, "sent ") || strings.Contains(p, "1970") {
		t.Errorf("zero time leaked a stamp: %q", p)
	}
}

func TestBuildPromptWithAttachment(t *testing.T) {
	att := &Attachment{Path: "/run/out/o1/incoming/42-report.pdf", Filename: "report.pdf", MimeType: "application/pdf", Size: 2048}

	// With a caption: the file block announces the path + description, and the
	// caption is still appended as the message.
	p := buildPrompt("/code", RespondRequest{DocDir: "/run/out/o1", Prompt: "summarize this", Attachment: att})
	if !strings.Contains(p, "/run/out/o1/incoming/42-report.pdf") {
		t.Errorf("missing attachment path: %q", p)
	}
	if !strings.Contains(p, "report.pdf, 2.0 KB, application/pdf") {
		t.Errorf("missing attachment description: %q", p)
	}
	if !strings.Contains(p, "untrusted") {
		t.Errorf("missing untrusted-content caveat: %q", p)
	}
	if !strings.HasSuffix(p, "summarize this") {
		t.Errorf("caption should be appended last: %q", p)
	}

	// Without a caption: a placeholder tells the model to decide what to do.
	p = buildPrompt("/code", RespondRequest{DocDir: "/run/out/o1", Attachment: att})
	if !strings.Contains(p, "no caption") {
		t.Errorf("empty-caption placeholder missing: %q", p)
	}

	// From a reply: the file block says so (still untrusted, still in the outbox).
	r := buildPrompt("/code", RespondRequest{DocDir: "/run/out/o1", Prompt: "what is this", Attachment: att, AttachmentFromReply: true})
	if !strings.Contains(r, "message you are replying to") || !strings.Contains(r, "untrusted") {
		t.Errorf("reply-attachment phrasing missing: %q", r)
	}
}

func TestBuildPromptDelegated(t *testing.T) {
	// A delegated /do task frames the content as untrusted and names its author.
	p := buildPrompt("/code", RespondRequest{DocDir: "/o", Prompt: "restart the adapter", Delegated: true, DelegatedAuthor: "9(@guest)"})
	if !strings.Contains(p, "delegated") || !strings.Contains(p, "9(@guest)") {
		t.Errorf("delegation framing/author missing: %q", p)
	}
	if !strings.Contains(p, "UNTRUSTED") {
		t.Errorf("delegated content should be framed as untrusted: %q", p)
	}
	if !strings.HasSuffix(p, "restart the adapter") {
		t.Errorf("task should be the trailing message: %q", p)
	}
	// A non-delegated request carries no delegation framing.
	q := buildPrompt("/code", RespondRequest{DocDir: "/o", Prompt: "hi"})
	if strings.Contains(q, "delegated") {
		t.Errorf("non-delegated prompt should not mention delegation: %q", q)
	}
}

func TestBuildPromptTranscriptDir(t *testing.T) {
	p := buildPrompt("/code", RespondRequest{DocDir: "/out", TranscriptScope: "/s/transcripts/42", Prompt: "hi"})
	if !strings.Contains(p, "Your transcript directory (this conversation's history, read-only): /s/transcripts/42") {
		t.Errorf("missing transcript-dir line: %q", p)
	}
	if !strings.Contains(p, "tg-recall") || !strings.Contains(p, "AK_TGCLAUDE_TRANSCRIPT_DIR") {
		t.Errorf("transcript line should mention the skill + env var: %q", p)
	}
	// Empty scope omits the block entirely.
	if q := buildPrompt("/code", RespondRequest{DocDir: "/out", Prompt: "hi"}); strings.Contains(q, "transcript directory") {
		t.Errorf("empty scope should add no transcript line: %q", q)
	}
}

func TestBuildPromptReplyToHint(t *testing.T) {
	p := buildPrompt("/code", RespondRequest{DocDir: "/out", Prompt: "hi", ReplyToMsgID: 5123})
	if !strings.Contains(p, "replies to an earlier message (msg 5123)") {
		t.Errorf("missing reply-to hint: %q", p)
	}
	if !strings.Contains(p, "UNTRUSTED reference") {
		t.Errorf("reply-to hint should carry the untrusted-reference frame: %q", p)
	}
	// No reply => no hint.
	if q := buildPrompt("/code", RespondRequest{DocDir: "/out", Prompt: "hi"}); strings.Contains(q, "replies to an earlier message") {
		t.Errorf("replyTo=0 should add no hint: %q", q)
	}
}

func TestStubResponderRepliesFixed(t *testing.T) {
	f := &fakeSender{}
	m := newTestMCP(t, f) // helper from mcp_test.go
	tok, err := m.Register(Route{ChatID: 5, ReplyTo: 2}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&stubResponder{}).Respond(context.Background(), RespondRequest{
		Prompt: "any question", MCPURL: m.URL(), MCPToken: tok,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.SessionID != "" {
		t.Errorf("stub should not bind a session, got %q", res.SessionID)
	}
	calls := f.snapshot()
	if len(calls) != 1 || calls[0].text != "i am here" {
		t.Fatalf("stub reply wrong: %+v", calls)
	}
	if calls[0].route.ChatID != 5 || calls[0].route.ReplyTo != 2 {
		t.Errorf("stub reply not routed via the token: %+v", calls[0].route)
	}
}

func TestParseResult(t *testing.T) {
	out := []byte(`{"type":"result","session_id":"abc-123","result":"Sent the answer.\nanswered","total_cost_usd":0.0123}`)
	sid, outcome, final, cost := parseResult(out)
	if sid != "abc-123" {
		t.Errorf("session_id = %q, want abc-123", sid)
	}
	if outcome != "answered" {
		t.Errorf("outcome = %q, want answered", outcome)
	}
	if final != "Sent the answer.\nanswered" {
		t.Errorf("final text = %q", final)
	}
	if cost != 0.0123 {
		t.Errorf("total_cost_usd = %v, want 0.0123", cost)
	}
	if s, o, f, c := parseResult([]byte("not json")); s != "" || o != "" || f != "" || c != 0 {
		t.Errorf("malformed => %q/%q/%q/%v, want empty", s, o, f, c)
	}
}

func TestParseOutcome(t *testing.T) {
	// Exact last line wins.
	if got := parseOutcome("did stuff\nrefused"); got != "refused" {
		t.Errorf("last-line exact => %q", got)
	}
	// Trailing punctuation / markdown tolerated.
	if got := parseOutcome("ok\n**problematic**"); got != "problematic" {
		t.Errorf("punctuation-wrapped => %q", got)
	}
	// None present.
	if got := parseOutcome("here is your answer"); got != "" {
		t.Errorf("no outcome => %q, want empty", got)
	}
	// Fallback: last occurrence anywhere when the last line isn't exact.
	if got := parseOutcome("I answered it fully. Everything is fine now."); got != "answered" {
		t.Errorf("fallback scan => %q, want answered", got)
	}
}

func TestStdoutTail(t *testing.T) {
	if got := stdoutTail(nil); got != "(no stdout)" {
		t.Errorf("empty => %q, want (no stdout)", got)
	}
	if got := stdoutTail([]byte("  boom\n")); got != "boom" {
		t.Errorf("trim => %q, want boom", got)
	}
	// Long output keeps the LAST bytes (where the error text lands).
	long := strings.Repeat("x", 5000) + "END"
	got := stdoutTail([]byte(long))
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "END") {
		t.Errorf("tail => %q, want …-prefixed suffix ending END", got[:20])
	}
}
