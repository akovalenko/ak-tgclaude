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
	client        *Client // getUpdates
	sender        Sender  // sendMessage/sendDocument (= client in production)
	store         *SessionStore
	resp          Responder
	outboxRoot    string // writable root under which per-invocation outbox dirs are created
	pollTimeout   int
	maxConcurrent int // cap on responders running at once
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

	if isClearCommand(m.Text) {
		if err := d.store.Clear(m.Chat.ID); err != nil {
			log.Printf("ak-tgclaude: clear %d: %v", m.Chat.ID, err)
		}
		if _, err := d.sender.SendMessage(ctx, route, "Контекст очищен.", "", false); err != nil {
			log.Printf("ak-tgclaude: clear ack %d: %v", m.Chat.ID, err)
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

// isClearCommand reports whether text is the /clear command (optionally
// addressed to the bot, e.g. "/clear@mybot").
func isClearCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "/clear" || strings.HasPrefix(fields[0], "/clear@")
}

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
// SIGTERM.
func runDispatch(args []string) {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(2)
	}

	store, err := LoadSessionStore(cfg.StateDir)
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
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
			os.Exit(1)
		}
		resp = &claudeResponder{agent: cfg.Agent, cwd: cwd, project: cfg.Project, cacheDir: cacheDir}
	}

	d := &Dispatcher{
		client:        client,
		sender:        client,
		store:         store,
		resp:          resp,
		outboxRoot:    outboxRoot,
		pollTimeout:   defaultPollTimeout,
		maxConcurrent: cfg.MaxConcurrent,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kind := "fixed"
	if ephemeral {
		kind = "ephemeral"
	}
	log.Printf("ak-tgclaude: dispatch: responder=%s cwd=%s (%s) max_concurrent=%d state=%s token=%s",
		cfg.Responder, cwd, kind, cfg.MaxConcurrent, cfg.StateDir, redact(cfg.BotToken))

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
// ephemeral. A configured Cwd is fixed (created if needed, kept on exit);
// otherwise a pseudo-random dir is created under the runtime base.
func resolveResponderCwd(cfg *Config) (dir string, ephemeral bool, err error) {
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
