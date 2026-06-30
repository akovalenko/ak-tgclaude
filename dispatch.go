package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultPollTimeout is the getUpdates long-poll timeout (seconds). The HTTP
// client timeout must exceed it.
const defaultPollTimeout = 30

// Dispatcher is the long-lived process: it long-polls Telegram, routes each
// update to a responder, and delivers the responder's outbound messages.
type Dispatcher struct {
	client      *Client // getUpdates
	sender      Sender  // sendMessage/sendDocument (= client in production)
	store       *SessionStore
	resp        Responder
	runtimeBase string // where per-invocation outbox dirs are created
	pollTimeout int
}

// Run long-polls and processes updates sequentially until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	offset := d.store.Offset()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		updates, err := d.client.GetUpdates(ctx, offset, d.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("ak-tgclaude: getUpdates: %v", err)
			if !sleep(ctx, 3*time.Second) {
				return ctx.Err()
			}
			continue
		}
		for _, u := range updates {
			d.handleUpdate(ctx, u)
			offset = u.UpdateID + 1
			if err := d.store.SetOffset(offset); err != nil {
				log.Printf("ak-tgclaude: persisting offset: %v", err)
			}
		}
	}
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

	if isClearCommand(m.Text) {
		if err := d.store.Clear(m.Chat.ID); err != nil {
			log.Printf("ak-tgclaude: clear %d: %v", m.Chat.ID, err)
		}
		if _, err := d.sender.SendMessage(ctx, route, "Контекст очищен.", "", false); err != nil {
			log.Printf("ak-tgclaude: clear ack %d: %v", m.Chat.ID, err)
		}
		return
	}

	outbox, err := os.MkdirTemp(d.runtimeBase, "outbox-")
	if err != nil {
		log.Printf("ak-tgclaude: outbox for chat %d: %v", m.Chat.ID, err)
		return
	}
	defer os.RemoveAll(outbox)

	// One drainer for this invocation's outbox, stopped after the responder exits.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveOutbox(ctx, outbox, route, d.sender, stop)
	}()

	sid, _ := d.store.SessionID(m.Chat.ID)
	res, err := d.resp.Respond(ctx, RespondRequest{
		Prompt:    m.Text,
		SessionID: sid,
		OutboxDir: outbox,
	})
	close(stop)
	<-done

	if err != nil {
		log.Printf("ak-tgclaude: responder for chat %d: %v", m.Chat.ID, err)
		return
	}
	if res.SessionID != "" {
		if err := d.store.SetSession(m.Chat.ID, res.SessionID); err != nil {
			log.Printf("ak-tgclaude: binding chat %d: %v", m.Chat.ID, err)
		}
	}
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
	// NOTE: the responder cwd (scaffold: generated settings.json + skills) is
	// provisioned by the `deploy` subcommand / go:embed, which is not wired yet.
	// Until then a live run needs a hand-provisioned cwd at this path.
	cwd := filepath.Join(cfg.StateDir, "responder-cwd")
	d := &Dispatcher{
		client:      client,
		sender:      client,
		store:       store,
		resp:        &claudeResponder{agent: cfg.Agent, cwd: cwd},
		runtimeBase: resolveRuntimeBase(cfg.RuntimeBase),
		pollTimeout: defaultPollTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("ak-tgclaude: dispatch: profile=%s project=%s state=%s token=%s",
		cfg.Profile, cfg.Project, cfg.StateDir, redact(cfg.BotToken))
	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "ak-tgclaude: dispatch: %v\n", err)
		os.Exit(1)
	}
}
