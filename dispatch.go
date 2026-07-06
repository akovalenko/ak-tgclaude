package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultPollTimeout is the getUpdates long-poll timeout (seconds). The HTTP
// client timeout must exceed it.
const defaultPollTimeout = 30

// typingInterval is how often the "typing" chat action is refreshed. Telegram
// clears the action after ~5s, so re-asserting well inside that window keeps it
// continuously visible.
const typingInterval = 4 * time.Second

// keepTyping shows the "typing…" chat action in chat until ctx is cancelled,
// refreshing it before Telegram's ~5s expiry. It sends once immediately (so the
// user gets feedback the moment their message lands) and then every
// typingInterval. Chat-action delivery is best-effort UX, so failures are only
// logged, and not while ctx is already cancelled (a cancelled send is expected).
func keepTyping(ctx context.Context, s Sender, chatID int64) {
	t := time.NewTicker(typingInterval)
	defer t.Stop()
	for {
		if err := s.SendChatAction(ctx, chatID, "typing"); err != nil && ctx.Err() == nil {
			log.Printf("ak-tgclaude: chat action chat=%d: %v", chatID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Dispatcher is the long-lived process: it long-polls Telegram, routes each
// update to a responder, and delivers the responder's outbound messages.
type Dispatcher struct {
	client           *Client    // getUpdates
	sender           Sender     // sendMessage/sendDocument (= client in production)
	mcp              *mcpServer // outbound transport: the responder's send_* tools deliver through here
	store            *SessionStore
	resp             Responder
	authz            Authorizer    // gates which Telegram users may use the bot
	outboxRoot       string        // writable root under which per-chat persistent outbox (doc/scratch) dirs live
	outboxTTL        time.Duration // idle-eviction TTL for a chat's persistent outbox (<=0 disables)
	pollTimeout      int
	maxConcurrent    int    // cap on responders running at once
	maxIncomingBytes int64  // cap on an incoming document download (bytes; = MaxIncomingMB<<20)
	helpText         string // reply to /help and /start
	helpParseMode    string // "" (plain) or "HTML" for the help reply
	bill             bool   // send the run's dollar cost as a "$n.nnn" message after each answer
	debug            bool   // log the responder's full final text after each run (troubleshooting)
	botUsername      string // own @username (lowercased, no @) from getMe; "" => @mention addressing disabled

	// persona injected via --append-system-prompt on a chat's FIRST spawn (frozen
	// for the session): the composed default, plus each configured user's resolved
	// override (an absent key => the default). See the persona type for the
	// text/selector split.
	defaultPersona persona
	personas       map[int64]persona
	// groupDefaultPersona is the persona injected on a GROUP chat's first spawn when
	// the group has no per-group override (a negative key in personas). A group's
	// persona is keyed by chat, not by whoever speaks first.
	groupDefaultPersona persona

	requireDelivery bool   // guard: if the responder sent nothing, re-prompt once (then fall back)
	undeliveredText string // fallback reply when the guard's re-prompt still delivered nothing ("" => none)

	// transcripts records every turn (nil => feature off). transcriptRoot mirrors
	// cfg.TranscriptRoot(); owner/ownerReadsAll drive the responder's read scope (the
	// owner reads the whole root, others only their own chat) — used in Phase 4.
	transcripts    *TranscriptStore
	transcriptRoot string
	owner          int64
	ownerReadsAll  bool

	// usage, when non-nil, appends one JSONL line per answered round (elapsed +
	// round-summed cost). Enabled solely by configuring usage_log; nil => off.
	usage *UsageLog
	// usageLogPath mirrors cfg.UsageLog ("" => off): the file the OWNER's responder is
	// granted read of (sandbox allowRead + env + prompt hint), and every other
	// responder is DENIED read of (per-invocation sandbox denyRead). Distinct from
	// `usage` (the writer) — this drives the reader-side access split in handleUpdate.
	usageLogPath string
}

// Run long-polls and dispatches updates to per-chat workers until ctx is
// cancelled. Different chats are handled concurrently (bounded by
// maxConcurrent); updates within one chat are serialized.
func (d *Dispatcher) Run(ctx context.Context) error {
	workers := newChatWorkers(ctx, d.handleUpdate, d.maxConcurrent)
	offset := d.store.Offset()
	for ctx.Err() == nil {
		updates, err := d.client.GetUpdates(ctx, offset, d.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("ak-tgclaude: getUpdates: %v", err)
			if !sleep(ctx, 3*time.Second) {
				break
			}
			continue
		}
		for _, u := range updates {
			workers.dispatch(u)
			offset = u.UpdateID + 1
			if err := d.store.SetOffset(offset); err != nil {
				log.Printf("ak-tgclaude: persisting offset: %v", err)
			}
		}
	}
	workers.wait()
	return ctx.Err()
}

// chatWorkers serializes updates per chat while running different chats
// concurrently, bounded by a global responder cap. A worker goroutine per chat
// drains that chat's queue in order; the semaphore caps how many run at once.
type chatWorkers struct {
	ctx    context.Context
	handle func(context.Context, Update)
	sem    chan struct{}

	mu      sync.Mutex
	workers map[int64]chan Update
	wg      sync.WaitGroup
}

func newChatWorkers(ctx context.Context, handle func(context.Context, Update), maxConcurrent int) *chatWorkers {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &chatWorkers{
		ctx:     ctx,
		handle:  handle,
		sem:     make(chan struct{}, maxConcurrent),
		workers: make(map[int64]chan Update),
	}
}

// dispatch routes an update to its chat's worker, creating one on first sight.
func (w *chatWorkers) dispatch(u Update) {
	if u.Message == nil {
		return // non-message update: no chat to serialize on
	}
	chat := u.Message.Chat.ID
	w.mu.Lock()
	ch, ok := w.workers[chat]
	if !ok {
		ch = make(chan Update, 128)
		w.workers[chat] = ch
		w.wg.Add(1)
		go w.serve(ch)
	}
	w.mu.Unlock()

	select {
	case ch <- u:
	case <-w.ctx.Done():
	}
}

// serve drains one chat's queue sequentially, each update taking a global slot.
func (w *chatWorkers) serve(ch chan Update) {
	defer w.wg.Done()
	for {
		select {
		case <-w.ctx.Done():
			return
		case u := <-ch:
			select {
			case w.sem <- struct{}{}:
			case <-w.ctx.Done():
				return
			}
			w.handle(w.ctx, u)
			<-w.sem
		}
	}
}

// wait blocks until all worker goroutines have exited (after ctx is done).
func (w *chatWorkers) wait() { w.wg.Wait() }

// redeliverPrompt is fed to the SAME session by the delivery guard when a
// responder turn delivered nothing: its final text is discarded, so the answer
// never reached the user. It nudges the model to actually send this time.
const redeliverPrompt = "Your previous turn ended without calling any send tool, so nothing reached the user " +
	"— your final message is only a status signal and is discarded, never delivered. Send your actual reply now " +
	"by calling mcp__tg__send_message (or send_code / send_document). If you meant to decline, tell the user so " +
	"via mcp__tg__send_message. Then end with the status word as usual."

// incomingText is the user's text for the prompt: a plain message's Text, or a
// media message's Caption (Telegram puts a document's caption there and leaves
// Text empty).
func incomingText(m *Message) string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// isEmptyMessage reports whether m carries nothing to act on or usefully record:
// no text, no caption, and no attachment. Telegram service messages (a member
// joining, the bot being added to a group), stickers, polls, and locations land
// here — the dispatcher ignores them entirely, so they neither spawn a responder
// nor leave an empty line in the transcript (noise for recall).
func isEmptyMessage(m *Message) bool {
	return incomingText(m) == "" && incomingFile(m) == nil
}

// messageSentAt is the message's Telegram send time as a local Time, or the zero
// Time when the field is absent (so the prompt omits the stamp rather than
// printing the 1970 epoch).
func messageSentAt(m *Message) time.Time {
	if m.Date <= 0 {
		return time.Time{}
	}
	return time.Unix(m.Date, 0)
}

// replyToID is the message_id the incoming message replies to, or 0 when it is not
// a reply. It threads into the transcript record (the thread edge) and the prompt
// hint.
func replyToID(m *Message) int64 {
	if m.ReplyTo != nil {
		return m.ReplyTo.MessageID
	}
	return 0
}

// attachMeta renders an incoming attachment as transcript metadata (no bytes), or
// nil when the message carried none. The kind mirrors incomingFile's two cases (a
// document, or a photo).
func attachMeta(m *Message, a *Attachment) []TranscriptAttach {
	if a == nil {
		return nil
	}
	kind := "document"
	if m.Document == nil && len(m.Photo) > 0 {
		kind = "photo"
	}
	return []TranscriptAttach{{Kind: kind, Name: a.Filename, Size: a.Size, Mime: a.MimeType}}
}

// attachMetaDeclared renders a message's incoming attachment as transcript
// metadata from its DECLARED fields (no download), or nil when it carries none.
// Used for group chatter we record but do not fetch — the size/name are the
// sender's declared values, not the fetched file's.
func attachMetaDeclared(m *Message) []TranscriptAttach {
	s := incomingFile(m)
	if s == nil {
		return nil
	}
	kind := "document"
	if m.Document == nil && len(m.Photo) > 0 {
		kind = "photo"
	}
	return []TranscriptAttach{{Kind: kind, Name: s.FileName, Size: s.FileSize, Mime: s.MimeType}}
}

// recordUserTurn appends one incoming user turn to the transcript (a no-op when
// the feature is off). meta is the attachment metadata to record (from the
// fetched Attachment on the spawn path, or declared for chatter we don't fetch).
// User/Name attribute the turn — essential in a group, where one chat mixes many
// speakers. The dispatcher is the only writer (it holds the trusted chat_id).
func (d *Dispatcher) recordUserTurn(m *Message, meta []TranscriptAttach) {
	d.recordUserTurnAs(m, incomingText(m), meta)
}

// recordUserTurnAs records the turn with an explicit text — used by the /do path,
// which records the resolved task text rather than the literal "/do …" command.
func (d *Dispatcher) recordUserTurnAs(m *Message, text string, meta []TranscriptAttach) {
	if d.transcripts == nil {
		return
	}
	// meta.json identity describes the CHAT: a group is named by its own title/handle
	// (per-speaker authorship already rides in the record's User/Name/Username fields),
	// a private chat by its single partner.
	var ident *ChatIdentity
	if m.Chat.isGroup() {
		ident = &ChatIdentity{Type: m.Chat.Type, Title: m.Chat.Title, Username: m.Chat.Username}
	} else if m.From != nil {
		ident = &ChatIdentity{Type: m.Chat.Type, Username: m.From.Username, FirstName: m.From.FirstName}
	}
	rec := TranscriptRecord{
		MsgID:    m.MessageID,
		TS:       messageSentAt(m),
		Role:     "user",
		ReplyTo:  replyToID(m),
		Text:     text,
		Attach:   meta,
		User:     userID(m.From),
		Name:     userFirstName(m.From),
		Username: userHandle(m.From),
	}
	if err := d.transcripts.Append(m.Chat.ID, rec, ident); err != nil {
		log.Printf("ak-tgclaude: transcript(user) chat=%d msg=%d: %v", m.Chat.ID, m.MessageID, err)
	}
}

// doTask is a /do command resolved to a task: the prompt text, the message it (and
// any attachment) come from, the author of the content, and whether the content
// was delegated from a DIFFERENT author (a reply) versus typed inline.
type doTask struct {
	text      string   // the resolved task prompt
	src       *Message // message the task/attachment come from (the /do message, or the replied-to one)
	author    *User    // who authored the content (for attribution)
	delegated bool     // true when the content came from a replied-to message
}

// parseDoTask resolves a /do message. Inline text after "/do" is the task
// (authored by the commander). Otherwise a reply's content is the task (authored
// by the replied-to sender — delegation). A bare /do with neither yields empty.
func parseDoTask(m *Message) doTask {
	if inline := stripLeadingToken(m.Text); inline != "" {
		return doTask{text: inline, src: m, author: m.From}
	}
	if m.ReplyTo != nil {
		return doTask{text: incomingText(m.ReplyTo), src: m.ReplyTo, author: m.ReplyTo.From, delegated: true}
	}
	return doTask{src: m, author: m.From}
}

// stripLeadingToken drops the first whitespace-delimited token of s (the "/do" or
// "/do@bot" command word) and returns the trimmed remainder, or "" if there is
// only the token.
func stripLeadingToken(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n\r")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[i:])
}

// handleUpdate processes one update: /clear resets the chat's session; anything
// else is answered by a responder whose outbound messages are delivered to the
// chat (replying to the incoming message).
func (d *Dispatcher) handleUpdate(ctx context.Context, u Update) {
	if u.Message == nil {
		return // non-message update (we only request "message", but be safe)
	}
	m := u.Message
	route := Route{ChatID: m.Chat.ID, ReplyTo: m.MessageID}
	group := m.Chat.isGroup()

	// Ignore a message with nothing to act on — a Telegram service message (a member
	// joining, the bot being added), a sticker, a poll. It is neither answered nor
	// recorded (an empty transcript line is only noise for recall). Runs before the
	// access gate so a service message from anyone, authorized or not, is dropped.
	if isEmptyMessage(m) {
		return
	}

	// Access gate (runs before any command or the responder). A sender not on the
	// whitelist is not answered. In a GROUP their message is still recorded (privacy-off
	// chatter is context for recall) but the bot stays silent — no reply, so it leaks
	// neither its presence nor the gate to strangers. In a PRIVATE chat, /start and
	// /help get a "no access for id N" line so the person can report the id to be
	// whitelisted.
	if uid := userID(m.From); !d.authz.Allowed(uid) {
		if group {
			d.recordUserTurn(m, attachMetaDeclared(m))
			log.Printf("ak-tgclaude: denied(group) chat=%d user=%s msg=%d", m.Chat.ID, userLabel(m.From), m.MessageID)
			return
		}
		if isSlashCommand(m.Text, "help") || isSlashCommand(m.Text, "start") {
			msg := fmt.Sprintf("no access for id %d", uid)
			if _, err := d.sender.SendMessage(ctx, route, msg, "", false); err != nil {
				log.Printf("ak-tgclaude: no-access reply %d: %v", m.Chat.ID, err)
			}
		}
		log.Printf("ak-tgclaude: denied chat=%d user=%s msg=%d", m.Chat.ID, userLabel(m.From), m.MessageID)
		return
	}

	if isClearCommand(m.Text) {
		if p, ok := d.store.Outbox(m.Chat.ID); ok {
			if err := os.RemoveAll(p); err != nil {
				log.Printf("ak-tgclaude: clear outbox %d: %v", m.Chat.ID, err)
			}
		}
		if err := d.store.Clear(m.Chat.ID); err != nil {
			log.Printf("ak-tgclaude: clear %d: %v", m.Chat.ID, err)
		}
		if _, err := d.sender.SendMessage(ctx, route, "Контекст очищен.", "", false); err != nil {
			log.Printf("ak-tgclaude: clear ack %d: %v", m.Chat.ID, err)
		}
		return
	}

	// /help and /start are answered by the dispatcher itself (no model spawn):
	// the configured help_text, or a generic built-in. Telegram sends /start when
	// a user first opens the bot, so intercepting it keeps that off the responder.
	if isSlashCommand(m.Text, "help") || isSlashCommand(m.Text, "start") {
		if _, err := d.sender.SendMessage(ctx, route, d.helpText, d.helpParseMode, false); err != nil {
			log.Printf("ak-tgclaude: help %d: %v", m.Chat.ID, err)
		}
		return
	}

	// In a GROUP, a message that is neither a command nor addressed to the bot (an
	// @mention or /do) is recorded as context but does NOT spawn a responder — the bot
	// listens to the room without answering every line. Addressed messages fall through
	// to the spawn path below, which records them there (so no double-record here).
	if group && !d.addressed(m) {
		d.recordUserTurn(m, attachMetaDeclared(m))
		return
	}

	// /do delegation: the task is the inline text after /do, or (on a reply) the
	// replied-to message's content — authored by that sender but endorsed for
	// execution by the authorized commander (already past the access gate). A bare /do
	// with neither is a usage hint. Reply-form answers thread under the ORIGINAL
	// message, and the content is inlined as the task (so no "recall msg N" hint).
	promptText := incomingText(m)
	replyHint := replyToID(m)
	var delegated bool
	var delegatedAuthor string
	if isSlashCommand(m.Text, "do") {
		task := parseDoTask(m)
		if task.text == "" {
			if _, err := d.sender.SendMessage(ctx, route, "Пусто: ответьте командой /do на сообщение, либо напишите /do <задача>.", "", false); err != nil {
				log.Printf("ak-tgclaude: /do usage reply chat=%d: %v", m.Chat.ID, err)
			}
			return
		}
		promptText = task.text
		route.ReplyTo = task.src.MessageID
		if task.delegated {
			delegated = true
			delegatedAuthor = userLabel(task.author)
			replyHint = 0 // content is inlined as the task; no recall-by-id hint needed
		}
	}

	// Incoming media (a document or a photo) is downloaded into the outbox for the
	// responder. The effective source is the message's OWN attachment; if it has none
	// but replies to a message that carried one, that replied-to file is used instead
	// (transcripts store only metadata, so re-fetching the replied-to file_id is the
	// only path back to its bytes — the same rule for a plain reply-with-a-question and
	// for /do on a file). The message's own file always wins over the replied-to one.
	incoming, fromReply, srcMsgID := effectiveIncoming(m)
	// Reject an oversized OWN attachment up front (the file IS the request; the bot
	// can't fetch it past getFile's ~20 MB ceiling). An oversized REPLIED-TO file is
	// auxiliary context — drop it and answer from the text rather than abort.
	if incoming != nil && d.maxIncomingBytes > 0 && incoming.FileSize > d.maxIncomingBytes {
		if fromReply {
			log.Printf("ak-tgclaude: replied-to file too big chat=%d size=%d cap=%d — dropping", m.Chat.ID, incoming.FileSize, d.maxIncomingBytes)
			incoming = nil
		} else {
			mb := d.maxIncomingBytes >> 20
			if _, err := d.sender.SendMessage(ctx, route, fmt.Sprintf("Файл слишком большой — максимум %d МБ.", mb), "", false); err != nil {
				log.Printf("ak-tgclaude: too-big reply chat=%d: %v", m.Chat.ID, err)
			}
			log.Printf("ak-tgclaude: incoming file too big chat=%d size=%d cap=%d", m.Chat.ID, incoming.FileSize, d.maxIncomingBytes)
			return
		}
	}

	// Reap idle chats' persistent outboxes (TTL); skip this chat, whose record is
	// about to be refreshed. Runs on dispatch (no separate timer) — simplest policy.
	d.evictExpiredOutboxes(m.Chat.ID)

	// Persistent per-session writable dir for the responder's attachments and scratch
	// (its Write-tool scope): reattached across this chat's turns so the model needn't
	// rebuild what it built earlier. Reuse the chat's recorded outbox if it still
	// exists, else mint a fresh one and record its path (keyed by chat — the fresh
	// session id isn't known until after the run). Under outboxRoot as before, so the
	// scaffold's sibling-isolation (deny-read of outboxRoot, own dir carved back per
	// invocation) is unchanged.
	docDir, err := d.resolveOutbox(m.Chat.ID)
	if err != nil {
		log.Printf("ak-tgclaude: outbox for chat %d: %v", m.Chat.ID, err)
		return
	}
	// Reap only the sandboxed tmp Claude creates under TMPDIR (= docDir): TMPDIR is the
	// BASE and Claude appends /claude-<uid>. Scratch dies each turn; the rest of the
	// outbox (the model's builds/checkouts) persists. One defer at return covers the
	// main run and the delivery-guard re-prompt (both share docDir).
	defer os.RemoveAll(filepath.Join(docDir, fmt.Sprintf("claude-%d", os.Getuid())))

	// Incoming file: download it into the outbox so the responder can read or Edit
	// it (an image, too — the Read tool renders it). On failure, tell the user and
	// stop — a silent drop would leave them waiting on a file the model never saw.
	var attach *Attachment
	if incoming != nil {
		attach, err = d.fetchIncoming(ctx, incoming, srcMsgID, docDir)
		if err != nil {
			log.Printf("ak-tgclaude: fetch incoming file chat=%d msg=%d fromReply=%v: %v", m.Chat.ID, srcMsgID, fromReply, err)
			if fromReply {
				// The replied-to file is auxiliary — warn and continue with the text.
				if _, e := d.sender.SendMessage(ctx, route, "Не удалось поднять файл из того сообщения — отвечаю по тексту.", "", false); e != nil {
					log.Printf("ak-tgclaude: reply-fetch-warn chat=%d: %v", m.Chat.ID, e)
				}
				attach = nil
			} else {
				// The own attachment is the request itself — a silent drop would leave the
				// user waiting on a file the model never saw.
				if _, e := d.sender.SendMessage(ctx, route, "Не удалось скачать вложение.", "", false); e != nil {
					log.Printf("ak-tgclaude: fetch-fail reply chat=%d: %v", m.Chat.ID, e)
				}
				return
			}
		}
	}

	// Record the user's turn BEFORE spawning the responder: so this turn is itself
	// recallable, and so the chat's transcript subdir exists for the read scope. Record
	// the OWN attachment's metadata only — a replied-to file was already recorded under
	// its original message, so it is not re-attached here. Covers a private turn and an
	// addressed group turn alike; unaddressed group chatter was recorded at the gate.
	var recMeta []TranscriptAttach
	if !fromReply {
		recMeta = attachMeta(m, attach)
	}
	d.recordUserTurnAs(m, promptText, recMeta)

	// Mint this invocation's capability token: the MCP server resolves the route
	// (chat/reply) from it, so the responder's send_* calls carry no chat_id and
	// cannot retarget. Invalidated on responder exit.
	token, err := d.mcp.Register(route, docDir)
	if err != nil {
		log.Printf("ak-tgclaude: mcp register chat %d: %v", m.Chat.ID, err)
		return
	}
	defer d.mcp.Unregister(token)

	ulabel := userLabel(m.From)
	log.Printf("ak-tgclaude: launch responder chat=%d user=%s msg=%d", m.Chat.ID, ulabel, m.MessageID)

	start := time.Now()
	sid, _ := d.store.SessionID(m.Chat.ID)
	// On a FRESH spawn, compose+inject this user's persona (it freezes into the
	// session); on resume it is already there, so omit it.
	var appendPrompt string
	if sid == "" {
		var p persona
		if m.Chat.isGroup() {
			// A group's persona is keyed by the group (chat id), not the sender — so it
			// is deterministic regardless of who speaks first.
			p = d.personaForChat(m.Chat.ID)
		} else {
			var uid int64
			if m.From != nil {
				uid = m.From.ID
			}
			p = d.personaFor(uid)
		}
		appendPrompt = p.text
		// With --debug, dump the persona this user resolved to as it is injected: the
		// crisp selector label (e.g. [normal] vs [norefuse introspect]) answers "which
		// stance did this account get", and the composed --append-system-prompt text
		// below it is the exact dynamic prompt. Only on a fresh spawn — on resume the
		// persona is already frozen into the session and not re-injected.
		if d.debug {
			log.Printf("ak-tgclaude: persona chat=%d user=%s selectors=%v — injected as --append-system-prompt:\n%s",
				m.Chat.ID, ulabel, p.selectors, appendPrompt)
		}
	}
	// This invocation's transcript read scope: the owner reads the whole root
	// (cross-chat analytics), everyone else only their own chat. Empty when the
	// feature is off. The identity comes from the trusted from.id, never the model.
	var transcriptScope string
	if d.transcriptRoot != "" {
		if m.From != nil && d.owner != 0 && m.From.ID == d.owner && d.ownerReadsAll {
			transcriptScope = d.transcriptRoot
		} else {
			transcriptScope = filepath.Join(d.transcriptRoot, strconv.FormatInt(m.Chat.ID, 10))
		}
	}
	// Usage-log access split (feature on when usageLogPath != ""): the owner is granted
	// read of the file, everyone else is denied it (buildInvocationSettings emits
	// allowRead vs denyRead accordingly). Identity comes from the trusted from.id, never
	// the model. The file appears in NO static setting — the per-invocation grant is the
	// whole access story, so a non-owner cannot grep it even though read is default-open.
	usageLogOwner := m.From != nil && d.owner != 0 && m.From.ID == d.owner
	res, err := d.respondWithTyping(ctx, m.Chat.ID, RespondRequest{
		Prompt:              promptText,
		SentAt:              messageSentAt(m),
		Attachment:          attach,
		SessionID:           sid,
		DocDir:              docDir,
		MCPURL:              d.mcp.URL(),
		MCPToken:            token,
		AppendSystemPrompt:  appendPrompt,
		TranscriptScope:     transcriptScope,
		UsageLogPath:        d.usageLogPath,
		UsageLogOwner:       usageLogOwner,
		ReplyToMsgID:        replyHint,
		Delegated:           delegated,
		DelegatedAuthor:     delegatedAuthor,
		AttachmentFromReply: fromReply,
	})
	dur := time.Since(start).Round(time.Millisecond)

	if err != nil {
		log.Printf("ak-tgclaude: responder done chat=%d user=%s msg=%d FAILED in %s: %v",
			m.Chat.ID, ulabel, m.MessageID, dur, err)
		return
	}
	log.Printf("ak-tgclaude: responder done chat=%d user=%s msg=%d outcome=%s in %s",
		m.Chat.ID, ulabel, m.MessageID, outcomeField(res), dur)
	// In debug, dump the responder's full final text — in a normal run it is only
	// the status word, but with --debug the responder is asked for a full account
	// of what happened (e.g. whether the send tools were available), which is the
	// window into a run that emitted nothing to Telegram.
	if d.debug {
		if final := strings.TrimSpace(res.FinalText); final != "" {
			log.Printf("ak-tgclaude: responder final text chat=%d:\n%s", m.Chat.ID, final)
		}
	}
	if res.SessionID != "" {
		// If a resumed session was replaced by a new one (the old one expired), the
		// persistent outbox belongs to the old session's context — which the new
		// session doesn't remember — so wipe it; a fresh one is minted next turn.
		if prev, ok := d.store.SessionID(m.Chat.ID); ok && prev != "" && prev != res.SessionID {
			if p, ok := d.store.Outbox(m.Chat.ID); ok {
				if err := os.RemoveAll(p); err != nil {
					log.Printf("ak-tgclaude: reset outbox chat %d: %v", m.Chat.ID, err)
				}
			}
			if err := d.store.SetOutbox(m.Chat.ID, ""); err != nil {
				log.Printf("ak-tgclaude: reset outbox record chat %d: %v", m.Chat.ID, err)
			}
		}
		d.bindSession(m.Chat.ID, res.SessionID)
	}

	// Round accounting: the run's cost, to which a delivery-guard re-prompt (below)
	// adds its own — one round, one summed figure for both --bill and the usage log.
	roundCost := res.CostUSD

	// Delivery guard: the responder's final text is only a status signal (discarded),
	// so an answer reaches the user ONLY through a send tool. A weaker model sometimes
	// ends without calling one, dumping its answer into that discarded text. If this
	// invocation delivered nothing, re-prompt the SAME session once to actually send;
	// if it still delivers nothing, fall back to undeliveredText (when set). Keyed on
	// the per-invocation delivered count the MCP server tracks; token/docDir are still
	// registered here (their defers run at return), so the re-prompt reuses the route.
	if d.requireDelivery && d.mcp.DeliveredCount(token) == 0 {
		log.Printf("ak-tgclaude: no delivery chat=%d user=%s msg=%d — re-prompting the session", m.Chat.ID, ulabel, m.MessageID)
		resumeID := res.SessionID
		if resumeID == "" {
			resumeID = sid
		}
		res2, err := d.respondWithTyping(ctx, m.Chat.ID, RespondRequest{
			Prompt:          redeliverPrompt,
			SessionID:       resumeID,
			DocDir:          docDir,
			MCPURL:          d.mcp.URL(),
			MCPToken:        token,
			TranscriptScope: transcriptScope,
		})
		roundCost += res2.CostUSD // fold the re-prompt's cost into the round (0 if it errored)
		if err != nil {
			log.Printf("ak-tgclaude: redeliver chat=%d user=%s msg=%d FAILED: %v", m.Chat.ID, ulabel, m.MessageID, err)
		} else if res2.SessionID != "" {
			d.bindSession(m.Chat.ID, res2.SessionID)
		}
		if d.mcp.DeliveredCount(token) == 0 {
			log.Printf("ak-tgclaude: still no delivery chat=%d user=%s msg=%d after re-prompt", m.Chat.ID, ulabel, m.MessageID)
			if d.undeliveredText != "" {
				if _, err := d.sender.SendMessage(ctx, route, d.undeliveredText, "", false); err != nil {
					log.Printf("ak-tgclaude: undelivered fallback chat=%d: %v", m.Chat.ID, err)
				}
			}
		}
	}

	// Whole-round wall time, captured before the (dispatcher-side) bill/usage sends so
	// their own latency doesn't inflate "how long the model took"; covers any re-prompt.
	roundElapsed := time.Since(start)

	if d.bill {
		if line, ok := billLine(roundCost); ok {
			if _, err := d.sender.SendMessage(ctx, route, line, "", false); err != nil {
				log.Printf("ak-tgclaude: bill %d: %v", m.Chat.ID, err)
			}
		}
	}

	if d.usage != nil {
		if err := d.usage.Append(start, m.Chat.ID, userID(m.From), m.MessageID, roundElapsed, roundCost); err != nil {
			log.Printf("ak-tgclaude: usage log chat=%d: %v", m.Chat.ID, err)
		}
	}
}

// respondWithTyping runs one responder invocation with the "typing…" chat
// action kept refreshed for its whole duration. Each delivered message clears
// the action, so the periodic refresh re-asserts it in the gaps of a
// multi-message answer.
func (d *Dispatcher) respondWithTyping(ctx context.Context, chatID int64, req RespondRequest) (RespondResult, error) {
	typingCtx, stopTyping := context.WithCancel(ctx)
	defer stopTyping()
	go keepTyping(typingCtx, d.sender, chatID)
	return d.resp.Respond(ctx, req)
}

// bindSession records chat→session in the durable store. A persist failure is
// logged, not fatal — the reply already went out; worst case the next turn
// starts a fresh session.
func (d *Dispatcher) bindSession(chat int64, sessionID string) {
	if err := d.store.SetSession(chat, sessionID); err != nil {
		log.Printf("ak-tgclaude: binding chat %d: %v", chat, err)
	}
}

// billLine renders the round's dollar cost for the --bill message: a bare "$n.nnn"
// at tenth-of-a-cent precision (the round is the run plus any delivery-guard
// re-prompt, summed by the caller). ok is false when the cost is absent or rounds
// to zero (total_cost_usd null/0 — e.g. a fully cached turn), so the dispatcher
// sends nothing rather than a meaningless "$0.000".
func billLine(costUSD float64) (string, bool) {
	if costUSD <= 0 {
		return "", false
	}
	line := fmt.Sprintf("$%.3f", costUSD)
	if line == "$0.000" {
		return "", false
	}
	return line, true
}

// outcomeField renders the outcome for the done log: the recognized word, or
// "?" plus a quoted snippet of the raw final text when the responder ended with
// something unrecognized (so the operator can see what it actually said).
func outcomeField(res RespondResult) string {
	if res.Outcome != "" {
		return res.Outcome
	}
	if text := strings.TrimSpace(res.FinalText); text != "" {
		return "? result=" + strconv.Quote(snippet(text, 100))
	}
	return "?"
}

// snippet returns s truncated to at most n runes, with an ellipsis if cut.
func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// userID returns the sender's Telegram id, or 0 when there is no sender (e.g. a
// channel post); id 0 is never whitelisted, so such updates are denied.
func userID(u *User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

// persona is one composed --append-system-prompt persona: the merged fragment
// text plus the selector labels it was composed from (e.g. [normal] vs
// [norefuse introspect]). The labels feed only the --debug persona dump, so the
// log shows which stance a user resolved to, not just the composed prose.
type persona struct {
	text      string
	selectors []string
}

// personaFor returns the persona for a user: the resolved per-user override
// when one is configured, else the composed default.
func (d *Dispatcher) personaFor(userID int64) persona {
	if p, ok := d.personas[userID]; ok {
		return p
	}
	return d.defaultPersona
}

// personaForChat returns the persona for a GROUP chat: the resolved per-group
// override (keyed by the group's negative chat id) when configured, else the
// composed group default. A group's persona is a property of the group, not of
// whoever speaks first.
func (d *Dispatcher) personaForChat(chatID int64) persona {
	if p, ok := d.personas[chatID]; ok {
		return p
	}
	return d.groupDefaultPersona
}

// userFirstName is a sender's first name for transcript attribution — always
// present for a real user, "" when there is no sender. The transcript keeps it
// apart from the @handle so a group reader sees the real name even when a handle
// exists (userHandle carries the handle separately).
func userFirstName(u *User) string {
	if u == nil {
		return ""
	}
	return u.FirstName
}

// userHandle is a sender's @username (without the @) for transcript attribution,
// or "" when they have none (or there is no sender).
func userHandle(u *User) string {
	if u == nil {
		return ""
	}
	return u.Username
}

// userLabel renders a message sender for logs: the numeric id, plus @username
// when present. "?" if there is no sender (e.g. channel posts).
func userLabel(u *User) string {
	if u == nil {
		return "?"
	}
	if u.Username != "" {
		return fmt.Sprintf("%d(@%s)", u.ID, u.Username)
	}
	return fmt.Sprintf("%d", u.ID)
}

// isSlashCommand reports whether text's first token is the /name command,
// optionally addressed to the bot (e.g. "/name@mybot") or carrying a payload
// (e.g. "/start deep-link"). Matching only the first token means "/name" must
// lead the message.
func isSlashCommand(text, name string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	cmd := "/" + name
	return fields[0] == cmd || strings.HasPrefix(fields[0], cmd+"@")
}

// isClearCommand reports whether text is the /clear command.
func isClearCommand(text string) bool { return isSlashCommand(text, "clear") }

// isUsernameByte reports whether b can appear inside a Telegram @username
// (letters lowercased, digits, underscore). Used to find the right edge of an
// @mention so "@bot" does not match inside "@bot2".
func isUsernameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z')
}

// mentionsBot reports whether the message text or caption @mentions this bot by
// username (case-insensitive), as a whole token — a trailing username byte (e.g.
// "@bot2" when we are "bot") does not count. Empty botUsername (getMe failed)
// disables the check. The mention is NOT stripped: the model sees the text as
// sent (a deliberate design choice).
func (d *Dispatcher) mentionsBot(m *Message) bool {
	if d.botUsername == "" {
		return false
	}
	hay := strings.ToLower(m.Text + " " + m.Caption)
	at := "@" + d.botUsername
	for i := 0; ; {
		j := strings.Index(hay[i:], at)
		if j < 0 {
			return false
		}
		k := i + j + len(at)
		if k == len(hay) || !isUsernameByte(hay[k]) {
			return true
		}
		i = k
	}
}

// addressed reports whether a group message is directed at the bot: an @mention
// or the /do command. Free chatter that is neither is recorded but not answered.
func (d *Dispatcher) addressed(m *Message) bool {
	return isSlashCommand(m.Text, "do") || d.mentionsBot(m)
}

// botCommands is the command menu uploaded via setMyCommands at startup. /start
// is handled too but conventionally not listed (clients surface it as START).
var botCommands = []BotCommand{
	{Command: "help", Description: "What this bot does and how to use it"},
	{Command: "clear", Description: "Start a fresh conversation (forget context)"},
}

// groupBotCommands is the command menu uploaded for the all_group_chats scope:
// /do (delegation) is the group-specific verb, plus /clear. This is only the
// autocomplete list clients show — NOT access control; the user-id gate still
// decides whether a command runs.
var groupBotCommands = []BotCommand{
	{Command: "do", Description: "Run this — reply to a message, or /do <task>"},
	{Command: "clear", Description: "Start a fresh conversation (forget context)"},
}

// defaultHelpText is the /help and /start reply when the operator sets no
// help_text. Deliberately domain-blind — it describes the bot's mechanics, not
// the project (a deployment supplies specifics via help_text).
const defaultHelpText = "Send me a question and I'll answer it from the project I'm set up for — " +
	"just type it normally, no command needed.\n\n" +
	"/clear — start a fresh conversation (I forget the earlier context)\n" +
	"/help — show this message"

// sleep waits for d or ctx cancellation; it reports whether the full delay
// elapsed (false if ctx was cancelled first).
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// resolveRuntimeBase picks the base dir for per-invocation outbox dirs.
func resolveRuntimeBase(configured string) string {
	if configured != "" {
		return configured
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

// runDispatch loads configuration and runs the dispatcher loop until SIGINT/
// SIGTERM. Failures are returned for main to report and exit-code.
func runDispatch(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return usageError{err}
	}

	store, err := LoadSessionStore(cfg.SessionDir(), cfg.EphemeralSessions)
	if err != nil {
		return err
	}

	// The transcript store (nil unless the feature is on). Created here so it can be
	// wired into both the user-side write (handleUpdate) and the bot-side write (the
	// MCP server, below).
	var transcripts *TranscriptStore
	if root := cfg.TranscriptRoot(); root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("transcript dir %s: %w", root, err)
		}
		transcripts = NewTranscriptStore(root)
		log.Printf("ak-tgclaude: transcripts on, root %s (owner_reads_all=%v)", root, cfg.OwnerReadsAllTranscripts())
	}

	// The usage log (nil unless usage_log is set): one JSONL line per answered round.
	var usage *UsageLog
	if p := cfg.UsageLog; p != "" {
		usage, err = NewUsageLog(p)
		if err != nil {
			return err
		}
		log.Printf("ak-tgclaude: usage log on, path %s", p)
	}

	client := NewClient(cfg.BotToken)

	// The outbound transport: a dispatcher-owned MCP server the responders deliver
	// through. Created before either responder kind (the stub calls it too). The
	// uploader (nil unless upload_command is set) gives send_document its large-file
	// fallback — run here, in the unsandboxed dispatcher.
	uploader := newUploader(cfg.UploadCommand, cfg.UploadThresholdMB, cfg.UploadMaxMB)
	mcp, err := newMCPServer(client, version, cfg.Debug, uploader)
	if err != nil {
		return err
	}
	defer mcp.Close()
	mcp.transcripts = transcripts // bot-side transcript append happens in the MCP send path
	mcp.overflow = cfg.Overflow   // oversized-text policy (spill | error) for the send path
	log.Printf("ak-tgclaude: mcp server listening at %s", mcp.URL())

	cwd, ephemeral, err := resolveResponderCwd(cfg)
	if err != nil {
		return err
	}
	// A static workdir/project is a pure function of canon: reset its contents on
	// every start so a removed wire-skill or stale scaffold file never lingers.
	// Wipe the CONTENTS, never the dir — trust in ~/.claude.json is keyed by path,
	// so keeping the dir keeps it trusted. (An ephemeral cwd is freshly made, so
	// there is nothing to reset.)
	if cfg.Workdir != "" {
		if err := resetDirContents(cwd); err != nil {
			return err
		}
		// Mark the static project trusted once so Claude Code honors its
		// permissions.allow (and registers Grep/Glob on a vanilla build). Non-fatal:
		// a failure leaves the responder untrusted (its prior state), which still
		// works, so log and continue rather than refuse to start.
		if err := markProjectTrusted(cwd); err != nil {
			log.Printf("ak-tgclaude: dispatch: marking %s trusted in ~/.claude.json: %v (responder runs untrusted)", cwd, err)
		}
	}
	// The outbox root lives OUTSIDE cwd (which the scaffold write-denies): a sibling
	// $workdir/outbox, an operator-set outbox_root (e.g. a tmpfs), or a disposable
	// temp beside an ephemeral cwd. Resolved AFTER cwd so it can be validated
	// against it.
	outboxRoot, outboxEphemeral, err := resolveOutboxRoot(cfg, cwd)
	if err != nil {
		return err
	}

	var resp Responder
	switch cfg.Responder {
	case ResponderStub:
		resp = &stubResponder{}
	default:
		// Materialize the responder scaffold (generated .claude/settings.json with
		// the literal runtime paths: sandbox, token deny-read, hook) and launch
		// the responder there with --setting-sources project. The cache dir is
		// pre-created (the sandbox can write under it but not create it, since its
		// parent is not writable) and injected into the responder's env.
		cacheDir := filepath.Join(cfg.StateDir, "cache")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return err
		}
		if err := materializeScaffold(cwd, cfg.scaffoldParams(cacheDir, outboxRoot)); err != nil {
			return err
		}
		// Fail fast if the selected agent was not materialized (e.g. a custom
		// --agent name with nothing shipping it): otherwise `claude -p --agent`
		// would silently find no agent. The built-in default always exists.
		agentFile := filepath.Join(cwd, ".claude", "agents", cfg.Agent+".md")
		if _, err := os.Stat(agentFile); err != nil {
			return fmt.Errorf("agent %q not materialized (%s); use the default %q (custom agents are not wired yet)", cfg.Agent, agentFile, defaultAgent)
		}
		// The hook's protected paths: operator deny_reads plus the token file (when it
		// lives in a config). The SAME paths the sandbox denies for Bash — sourced once
		// here from config, projected into the per-invocation file policy (env) and,
		// separately, the static sandbox denyRead.
		denyPaths := append([]string(nil), cfg.DenyRead...)
		if cfg.ConfigPath != "" {
			denyPaths = append(denyPaths, cfg.ConfigPath)
		}
		resp = &claudeResponder{agent: cfg.Agent, cwd: cwd, project: cfg.Project, cacheDir: cacheDir, debug: cfg.Debug, claudeArgs: cfg.ClaudeArgs, extraTools: cfg.Tools, denyPaths: denyPaths}
	}

	helpText := cfg.HelpText
	if strings.TrimSpace(helpText) == "" {
		helpText = defaultHelpText
	}
	helpParseMode := ""
	if cfg.HelpHTML {
		helpParseMode = "HTML"
	}

	var authz Authorizer
	accessDesc := fmt.Sprintf("%d users", len(cfg.AllowedUsers))
	switch {
	case cfg.Open:
		authz = openAccess{}
		accessDesc = "OPEN"
		log.Printf("ak-tgclaude: dispatch: OPEN ACCESS — every Telegram user is allowed (demo mode)")
	default:
		authz = newAllowList(cfg.AllowedUsers)
		if len(cfg.AllowedUsers) == 0 {
			log.Printf("ak-tgclaude: dispatch: no allowed_users — denying everyone; send /start to see your id, then whitelist it")
		}
	}

	// Precompute the persona injected via --append-system-prompt on a fresh spawn:
	// the composed default, plus each per-user override's composed persona. The
	// fragments were already validated (and read for their axes) at config load, so
	// a failure here is unexpected — fail fast rather than mid-run.
	defaultPersona, err := loadPolicies(cfg.Policies)
	if err != nil {
		return fmt.Errorf("composing default persona: %w", err)
	}
	// personaByUser holds a persona per override key. A positive key is a user; a
	// negative key is a specific group (its selectors were resolved against the group
	// base at config load). PersonaSelectors returns the resolved list for either.
	personaByUser := make(map[int64]persona, len(cfg.overrides))
	for uid := range cfg.overrides {
		sel := cfg.PersonaSelectors(uid)
		p, perr := loadPolicies(sel)
		if perr != nil {
			return fmt.Errorf("composing persona for id %d: %w", uid, perr)
		}
		personaByUser[uid] = persona{text: string(p), selectors: sel}
	}
	// The default persona injected in a group with no per-group override.
	groupDefaultText, err := loadPolicies(cfg.groupDefault)
	if err != nil {
		return fmt.Errorf("composing group persona: %w", err)
	}

	d := &Dispatcher{
		client:              client,
		sender:              client,
		mcp:                 mcp,
		store:               store,
		resp:                resp,
		authz:               authz,
		defaultPersona:      persona{text: string(defaultPersona), selectors: cfg.Policies},
		personas:            personaByUser,
		groupDefaultPersona: persona{text: string(groupDefaultText), selectors: cfg.groupDefault},
		outboxRoot:          outboxRoot,
		outboxTTL:           cfg.OutboxTTLDur(),
		pollTimeout:         defaultPollTimeout,
		maxConcurrent:       cfg.MaxConcurrent,
		maxIncomingBytes:    int64(cfg.MaxIncomingMB) << 20,
		helpText:            helpText,
		helpParseMode:       helpParseMode,
		bill:                cfg.Bill,
		debug:               cfg.Debug,

		requireDelivery: !cfg.AllowSilent,
		undeliveredText: cfg.UndeliveredText,

		transcripts:    transcripts,
		transcriptRoot: cfg.TranscriptRoot(),
		owner:          cfg.Owner,
		ownerReadsAll:  cfg.OwnerReadsAllTranscripts(),

		usage:        usage,
		usageLogPath: cfg.UsageLog,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Learn the bot's own @username for @mention addressing in groups (best-effort:
	// on failure /do still addresses the bot, so @mention detection just stays off).
	if me, merr := client.GetMe(ctx); merr != nil {
		log.Printf("ak-tgclaude: getMe failed: %v — @mention addressing disabled (use /do)", merr)
	} else if me.Username != "" {
		d.botUsername = strings.ToLower(me.Username)
		log.Printf("ak-tgclaude: bot @%s", me.Username)
	}

	// Publish the command menus (best-effort: the bot works without them). The default
	// menu covers private chats; a separate all_group_chats menu surfaces /do in groups.
	if err := client.SetMyCommands(ctx, botCommands, nil); err != nil {
		log.Printf("ak-tgclaude: setMyCommands: %v", err)
	}
	if err := client.SetMyCommands(ctx, groupBotCommands, &BotCommandScope{Type: "all_group_chats"}); err != nil {
		log.Printf("ak-tgclaude: setMyCommands(group): %v", err)
	}

	kind := "fixed"
	if ephemeral {
		kind = "ephemeral"
	}
	log.Printf("ak-tgclaude: dispatch: responder=%s cwd=%s (%s) max_concurrent=%d access=%s state=%s token=%s",
		cfg.Responder, cwd, kind, cfg.MaxConcurrent, accessDesc, cfg.SessionDir(), redact(cfg.BotToken))

	runErr := d.Run(ctx)

	// Ephemeral sessions don't survive a restart, so neither should their persistent
	// outboxes: wipe them on shutdown. (A disposable cwd, removed just below, would
	// also take them — but a FIXED cwd with ephemeral sessions keeps the cwd, so the
	// outboxes must be removed explicitly.)
	if store.Ephemeral() {
		for _, p := range store.Outboxes() {
			if err := os.RemoveAll(p); err != nil {
				log.Printf("ak-tgclaude: dispatch: removing ephemeral outbox %s: %v", p, err)
			}
		}
	}

	// An ephemeral cwd is disposable: remove it on shutdown. A fixed cwd is kept for
	// inspection.
	if ephemeral {
		if err := os.RemoveAll(cwd); err != nil {
			log.Printf("ak-tgclaude: dispatch: removing ephemeral cwd %s: %v", cwd, err)
		}
	}
	// The outbox root is now a sibling of cwd, not under it, so removing cwd no
	// longer takes it: dispose of a disposable outbox root explicitly (the ephemeral
	// case; a $workdir/outbox or an operator outbox_root is kept across restarts).
	if outboxEphemeral {
		if err := os.RemoveAll(outboxRoot); err != nil {
			log.Printf("ak-tgclaude: dispatch: removing ephemeral outbox root %s: %v", outboxRoot, err)
		}
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// resolveResponderCwd returns the responder launch dir and whether it is
// ephemeral. A configured Workdir uses its fixed $Workdir/project (created if
// needed; the caller resets+regenerates its contents from canon each start).
// Otherwise a pseudo-random dir is created under the runtime base and removed on
// exit.
func resolveResponderCwd(cfg *Config) (dir string, ephemeral bool, err error) {
	if cfg.Workdir != "" {
		project := filepath.Join(cfg.Workdir, "project")
		if err := os.MkdirAll(project, 0o700); err != nil {
			return "", false, fmt.Errorf("creating responder cwd %s: %w", project, err)
		}
		return project, false, nil
	}
	// The base can come from $XDG_RUNTIME_DIR / os.TempDir(), which skip the config
	// validatePath pass — but the ephemeral cwd's outbox lands in the sandbox
	// deny-read glob, so validate the RESOLVED base too (a glob metacharacter in
	// $TMPDIR would silently mis-scope that deny-read).
	base := resolveRuntimeBase(cfg.RuntimeBase)
	if err := validatePath("runtime_base", base); err != nil {
		return "", false, err
	}
	dir, err = os.MkdirTemp(base, "ak-tgclaude-cwd-")
	if err != nil {
		return "", false, fmt.Errorf("creating ephemeral cwd: %w", err)
	}
	return dir, true, nil
}

// resolveOutboxRoot returns the root under which per-chat outboxes live, and
// whether it is disposable (removed on shutdown). It MUST sit OUTSIDE the
// responder cwd: the scaffold write-denies cwd (denyWrite), so an outbox there
// would be unwritable — the whole point of keeping the outbox a sibling is that
// its allowWrite is a plain grant, never an allow nested inside cwd's deny.
//
//   - outbox_root configured  -> that path (operator-owned, e.g. a size-capped
//     tmpfs mount); kept across restarts. Rejected if under cwd.
//   - workdir set             -> $workdir/outbox, a sibling of $workdir/project;
//     kept across restarts (persistent outboxes reattach).
//   - otherwise (ephemeral)   -> a fresh temp dir beside the ephemeral cwd, under
//     the same validated base; disposable.
func resolveOutboxRoot(cfg *Config, cwd string) (root string, ephemeral bool, err error) {
	switch {
	case cfg.OutboxRoot != "":
		root = cfg.OutboxRoot
		if err := ensureOutsideCwd("outbox_root", root, cwd); err != nil {
			return "", false, err
		}
	case cfg.Workdir != "":
		root = filepath.Join(cfg.Workdir, "outbox")
	default:
		// The ephemeral cwd was made with os.MkdirTemp under a validatePath'd base;
		// its parent is that base, so a sibling temp there is validated too and never
		// lands inside cwd.
		root, err = os.MkdirTemp(filepath.Dir(cwd), "ak-tgclaude-outbox-")
		if err != nil {
			return "", false, fmt.Errorf("creating ephemeral outbox root: %w", err)
		}
		return root, true, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", false, fmt.Errorf("creating outbox root %s: %w", root, err)
	}
	return root, false, nil
}

// ensureOutsideCwd rejects a path equal to or nested under cwd. Both are absolute
// (resolvePath'd config / MkdirTemp results), so a lexical prefix test is exact.
func ensureOutsideCwd(field, path, cwd string) error {
	if path == cwd || strings.HasPrefix(path, cwd+string(os.PathSeparator)) {
		return fmt.Errorf("%s %q is under the responder cwd %q, which is write-denied in the sandbox; put the outbox in a sibling dir or a separate mount (e.g. a tmpfs)", field, path, cwd)
	}
	return nil
}

// resolveOutbox returns the chat's persistent working dir: its recorded outbox if
// that still exists on disk, else a freshly minted one whose path is recorded (so
// the next turn reattaches it). A recording failure is non-fatal — the dir works
// this turn, it just won't be remembered across a dispatcher restart.
func (d *Dispatcher) resolveOutbox(chat int64) (string, error) {
	if p, ok := d.store.Outbox(chat); ok {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, nil
		}
	}
	p, err := os.MkdirTemp(d.outboxRoot, "outbox-")
	if err != nil {
		return "", err
	}
	if err := d.store.SetOutbox(chat, p); err != nil {
		log.Printf("ak-tgclaude: recording outbox chat %d: %v", chat, err)
	}
	return p, nil
}

// evictExpiredOutboxes reaps the persistent outboxes of chats idle past the TTL
// (keeping `active`, served right now), removing each from disk. A no-op when the
// TTL is disabled (outboxTTL <= 0).
func (d *Dispatcher) evictExpiredOutboxes(active int64) {
	paths, err := d.store.EvictExpired(time.Now(), d.outboxTTL, active)
	if err != nil {
		log.Printf("ak-tgclaude: outbox eviction sweep: %v", err)
	}
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			log.Printf("ak-tgclaude: evicting outbox %s: %v", p, err)
		}
	}
}
