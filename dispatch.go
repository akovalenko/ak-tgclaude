package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
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
	client        *Client // getUpdates
	sender        Sender  // sendMessage/sendDocument (= client in production)
	store         *SessionStore
	resp          Responder
	authz         Authorizer // gates which Telegram users may use the bot
	outboxRoot    string     // writable root under which per-invocation outbox dirs are created
	pollTimeout   int
	maxConcurrent int    // cap on responders running at once
	helpText      string // reply to /help and /start
	helpParseMode string // "" (plain) or "HTML" for the help reply
	bill          bool   // send the run's dollar cost as a "$n.nnn" message after each answer
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

// handleUpdate processes one update: /clear resets the chat's session; anything
// else is answered by a responder whose outbound messages are delivered to the
// chat (replying to the incoming message).
func (d *Dispatcher) handleUpdate(ctx context.Context, u Update) {
	if u.Message == nil {
		return // non-message update (we only request "message", but be safe)
	}
	m := u.Message
	route := Route{ChatID: m.Chat.ID, ReplyTo: m.MessageID}

	// Access gate (runs before any command or the responder). A user not on the
	// whitelist gets a "no access for id N" line on /start and /help — so they can
	// report the id to be whitelisted — and is otherwise silently ignored (the bot
	// does not talk to strangers). The id in the reply is that user's own.
	if uid := userID(m.From); !d.authz.Allowed(uid) {
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

	outbox, err := os.MkdirTemp(d.outboxRoot, "outbox-")
	if err != nil {
		log.Printf("ak-tgclaude: outbox for chat %d: %v", m.Chat.ID, err)
		return
	}
	defer os.RemoveAll(outbox)

	ulabel := userLabel(m.From)
	log.Printf("ak-tgclaude: launch responder chat=%d user=%s msg=%d", m.Chat.ID, ulabel, m.MessageID)

	// Show "typing…" for the responder's whole lifetime: refreshed until the
	// responder returns. Each delivered message clears the action, so the next
	// refresh re-asserts it in the gaps of a multi-message answer.
	typingCtx, stopTyping := context.WithCancel(ctx)
	go keepTyping(typingCtx, d.sender, m.Chat.ID)

	// One drainer for this invocation's outbox, stopped after the responder exits.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveOutbox(ctx, outbox, route, d.sender, stop)
	}()

	start := time.Now()
	sid, _ := d.store.SessionID(m.Chat.ID)
	res, err := d.resp.Respond(ctx, RespondRequest{
		Prompt:    m.Text,
		SessionID: sid,
		OutboxDir: outbox,
	})
	stopTyping()
	close(stop)
	<-done
	dur := time.Since(start).Round(time.Millisecond)

	if err != nil {
		log.Printf("ak-tgclaude: responder done chat=%d user=%s msg=%d FAILED in %s: %v",
			m.Chat.ID, ulabel, m.MessageID, dur, err)
		return
	}
	log.Printf("ak-tgclaude: responder done chat=%d user=%s msg=%d outcome=%s in %s",
		m.Chat.ID, ulabel, m.MessageID, outcomeField(res), dur)
	if res.SessionID != "" {
		if err := d.store.SetSession(m.Chat.ID, res.SessionID); err != nil {
			log.Printf("ak-tgclaude: binding chat %d: %v", m.Chat.ID, err)
		}
	}
	if d.bill {
		if line, ok := billLine(res.CostUSD); ok {
			if _, err := d.sender.SendMessage(ctx, route, line, "", false); err != nil {
				log.Printf("ak-tgclaude: bill %d: %v", m.Chat.ID, err)
			}
		}
	}
}

// billLine renders the run's dollar cost for the --bill message: a bare "$n.nnn"
// at tenth-of-a-cent precision. ok is false when the cost is absent or rounds to
// zero (total_cost_usd null/0 — e.g. a fully cached turn), so the dispatcher
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

// botCommands is the command menu uploaded via setMyCommands at startup. /start
// is handled too but conventionally not listed (clients surface it as START).
var botCommands = []BotCommand{
	{Command: "help", Description: "What this bot does and how to use it"},
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

// checkBinaryOnPath verifies that the `ak-tgclaude` resolved on PATH is the same
// file as the running binary. The responder inherits this PATH and invokes
// `ak-tgclaude send` (and other self-calls) by bare name, so a mismatch — not
// installed, shadowed by an earlier PATH entry, or a stale copy — would route
// replies through the wrong binary. Everything downstream assumes they match, so
// the dispatcher fails fast here rather than discover it on the first reply.
func checkBinaryOnPath() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own path (os.Executable): %w", err)
	}
	onPath, err := exec.LookPath("ak-tgclaude")
	if err != nil {
		return fmt.Errorf("'ak-tgclaude' is not on PATH (%w); install it (e.g. `go install`) "+
			"so the responder's `send` resolves to this binary", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stat self %s: %w", self, err)
	}
	pathInfo, err := os.Stat(onPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", onPath, err)
	}
	if !os.SameFile(selfInfo, pathInfo) {
		return fmt.Errorf("the 'ak-tgclaude' on PATH (%s) is not this running binary (%s); "+
			"the responder resolves `send` by bare name via PATH, so they must be the same — "+
			"reinstall (e.g. `go install`) or fix PATH", onPath, self)
	}
	return nil
}

// runDispatch loads configuration and runs the dispatcher loop until SIGINT/
// SIGTERM.
func runDispatch(args []string) {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(2)
	}

	store, err := LoadSessionStore(cfg.SessionDir(), cfg.EphemeralSessions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(1)
	}

	client := NewClient(cfg.BotToken)

	cwd, ephemeral, err := resolveResponderCwd(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(1)
	}
	// A static workdir/project is a pure function of canon: reset its contents on
	// every start so a removed wire-skill or stale scaffold file never lingers.
	// Wipe the CONTENTS, never the dir — trust in ~/.claude.json is keyed by path,
	// so keeping the dir keeps it trusted. (An ephemeral cwd is freshly made, so
	// there is nothing to reset.)
	if cfg.Workdir != "" {
		if err := resetDirContents(cwd); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
			os.Exit(1)
		}
	}
	outboxRoot := filepath.Join(cwd, "outbox")
	if err := os.MkdirAll(outboxRoot, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(1)
	}

	var resp Responder
	switch cfg.Responder {
	case ResponderStub:
		resp = &stubResponder{}
	default:
		// The responder invokes `ak-tgclaude send` by bare name from its skills
		// (resolved via the inherited PATH — deliberately kept out of the skills so
		// the model isn't burdened with a path). That send is a local outbox drop
		// (no token, no network — the dispatcher delivers), but it must be THIS
		// binary or replies run on some stale/other ak-tgclaude. The hook is pinned
		// to an absolute path, but these self-calls are not, so fail fast if the
		// ak-tgclaude on PATH is not us.
		if err := checkBinaryOnPath(); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
			os.Exit(1)
		}
		// Materialize the responder scaffold (generated .claude/settings.json with
		// the literal runtime paths: sandbox, token deny-read, hook) and launch
		// the responder there with --setting-sources project. The cache dir is
		// pre-created (the sandbox can write under it but not create it, since its
		// parent is not writable) and injected into the responder's env.
		cacheDir := filepath.Join(cfg.StateDir, "cache")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
			os.Exit(1)
		}
		if err := materializeScaffold(cwd, scaffoldParams{
			CacheDir:   cacheDir,
			OutboxRoot: outboxRoot,
			TokenFile:  cfg.ConfigPath,
			NoRefuse:   cfg.NoRefuse,
			Project:    cfg.Project,
			WireSkills: cfg.WireSkills,
			DenyRead:   cfg.DenyRead,
			HookBinary: selfExePath(),
			BangBug:    cfg.BangBug,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
			os.Exit(1)
		}
		// Fail fast if the selected agent was not materialized (e.g. a custom
		// --agent name with nothing shipping it): otherwise `claude -p --agent`
		// would silently find no agent. The built-in default always exists.
		agentFile := filepath.Join(cwd, ".claude", "agents", cfg.Agent+".md")
		if _, err := os.Stat(agentFile); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: agent %q not materialized (%s); use the default %q (custom agents are not wired yet)\n", cfg.Agent, agentFile, defaultAgent)
			os.Exit(1)
		}
		resp = &claudeResponder{agent: cfg.Agent, cwd: cwd, project: cfg.Project, cacheDir: cacheDir}
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

	d := &Dispatcher{
		client:        client,
		sender:        client,
		store:         store,
		resp:          resp,
		authz:         authz,
		outboxRoot:    outboxRoot,
		pollTimeout:   defaultPollTimeout,
		maxConcurrent: cfg.MaxConcurrent,
		helpText:      helpText,
		helpParseMode: helpParseMode,
		bill:          cfg.Bill,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Publish the command menu (best-effort: the bot works without it).
	if err := client.SetMyCommands(ctx, botCommands); err != nil {
		log.Printf("ak-tgclaude: setMyCommands: %v", err)
	}

	kind := "fixed"
	if ephemeral {
		kind = "ephemeral"
	}
	log.Printf("ak-tgclaude: dispatch: responder=%s cwd=%s (%s) max_concurrent=%d access=%s state=%s token=%s",
		cfg.Responder, cwd, kind, cfg.MaxConcurrent, accessDesc, cfg.SessionDir(), redact(cfg.BotToken))

	runErr := d.Run(ctx)

	// An ephemeral cwd is disposable: remove it (and its outbox) on shutdown. A
	// fixed cwd is kept for inspection.
	if ephemeral {
		if err := os.RemoveAll(cwd); err != nil {
			log.Printf("ak-tgclaude: dispatch: removing ephemeral cwd %s: %v", cwd, err)
		}
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", runErr)
		os.Exit(1)
	}
}

// resolveResponderCwd returns the responder launch dir and whether it is
// ephemeral. A configured Workdir uses its fixed $Workdir/project (created if
// needed; the caller resets+regenerates its contents from canon each start). A
// configured Cwd is fixed too (created if needed, kept as-is on exit). Otherwise a
// pseudo-random dir is created under the runtime base and removed on exit.
func resolveResponderCwd(cfg *Config) (dir string, ephemeral bool, err error) {
	if cfg.Workdir != "" {
		project := filepath.Join(cfg.Workdir, "project")
		if err := os.MkdirAll(project, 0o700); err != nil {
			return "", false, fmt.Errorf("creating responder cwd %s: %w", project, err)
		}
		return project, false, nil
	}
	if cfg.Cwd != "" {
		if err := os.MkdirAll(cfg.Cwd, 0o700); err != nil {
			return "", false, fmt.Errorf("creating responder cwd %s: %w", cfg.Cwd, err)
		}
		return cfg.Cwd, false, nil
	}
	dir, err = os.MkdirTemp(resolveRuntimeBase(cfg.RuntimeBase), "ak-tgclaude-cwd-")
	if err != nil {
		return "", false, fmt.Errorf("creating ephemeral cwd: %w", err)
	}
	return dir, true, nil
}
