package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitFor polls until cond() or the deadline, so the concurrency tests do not
// depend on a fixed sleep.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func TestChatWorkersSerializePerChatParallelAcrossChats(t *testing.T) {
	var mu sync.Mutex
	var inflight, maxInflight, done int
	active := map[int64]bool{}
	order := map[int64][]int64{}
	sameChatConcurrent := false

	handle := func(_ context.Context, u Update) {
		chat := u.Message.Chat.ID
		mu.Lock()
		if active[chat] {
			sameChatConcurrent = true
		}
		active[chat] = true
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		order[chat] = append(order[chat], u.Message.MessageID)
		mu.Unlock()

		time.Sleep(15 * time.Millisecond)

		mu.Lock()
		inflight--
		active[chat] = false
		done++
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newChatWorkers(ctx, handle, 8, time.Minute)

	// 3 chats, 3 messages each; message_id encodes intra-chat order (0,1,2).
	for i := 0; i < 3; i++ {
		for chat := int64(1); chat <= 3; chat++ {
			w.dispatch(Update{UpdateID: int64(i*3) + chat,
				Message: &Message{MessageID: int64(i), Chat: Chat{ID: chat}}})
		}
	}

	if !waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return done == 9 }, 3*time.Second) {
		t.Fatalf("not all updates processed: done=%d", done)
	}
	cancel()
	w.wait()

	mu.Lock()
	defer mu.Unlock()
	if sameChatConcurrent {
		t.Errorf("two updates of the same chat ran concurrently")
	}
	if maxInflight < 2 {
		t.Errorf("expected cross-chat parallelism, maxInflight=%d", maxInflight)
	}
	for chat, ids := range order {
		for i, id := range ids {
			if id != int64(i) {
				t.Errorf("chat %d out of order: %v", chat, ids)
				break
			}
		}
	}
}

func TestChatWorkersRespectConcurrencyCap(t *testing.T) {
	var mu sync.Mutex
	var inflight, maxInflight, done int

	handle := func(_ context.Context, _ Update) {
		mu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		inflight--
		done++
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newChatWorkers(ctx, handle, 2, time.Minute) // cap = 2

	for chat := int64(1); chat <= 6; chat++ {
		w.dispatch(Update{UpdateID: chat, Message: &Message{MessageID: 1, Chat: Chat{ID: chat}}})
	}

	if !waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return done == 6 }, 3*time.Second) {
		t.Fatalf("not all updates processed: done=%d", done)
	}
	cancel()
	w.wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInflight > 2 {
		t.Errorf("concurrency cap violated: maxInflight=%d > 2", maxInflight)
	}
	if maxInflight < 2 {
		t.Errorf("cap not saturated (expected 2), maxInflight=%d", maxInflight)
	}
}

// TestChatWorkersEvictIdle is the M1 regression: an idle per-chat worker evicts
// itself so a stranger's DM / a one-off group (a worker created BEFORE the access
// gate) cannot leak a goroutine + channel for the process lifetime — and a later
// update recreates it, losing nothing.
func TestChatWorkersEvictIdle(t *testing.T) {
	done := make(chan struct{}, 2)
	handle := func(context.Context, Update) { done <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newChatWorkers(ctx, handle, 4, 20*time.Millisecond)

	w.dispatch(Update{UpdateID: 1, Message: &Message{MessageID: 1, Chat: Chat{ID: 7}}})
	<-done // handled
	if !waitFor(func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.workers) == 0 }, time.Second) {
		t.Fatalf("idle worker was not evicted")
	}
	// A later update recreates the worker and is handled — no update lost to eviction.
	w.dispatch(Update{UpdateID: 2, Message: &Message{MessageID: 2, Chat: Chat{ID: 7}}})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("update after eviction not handled (worker not recreated)")
	}
}

func TestChatWorkersIgnoresNonMessage(t *testing.T) {
	called := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newChatWorkers(ctx, func(context.Context, Update) { called = true }, 4, time.Minute)
	w.dispatch(Update{UpdateID: 1}) // Message == nil
	time.Sleep(30 * time.Millisecond)
	if called {
		t.Errorf("non-message update should not be handled")
	}
}
