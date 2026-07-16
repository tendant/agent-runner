package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingGateway records every message text passed to Handle, in order, and
// always short-circuits (ok=true) so handleMessage never touches
// conversation state or starts an agent — isolating processEventBatch's
// skip/cap decision from the rest of the message pipeline.
type countingGateway struct {
	mu    sync.Mutex
	texts []string
}

func (g *countingGateway) Handle(text string, _ func(string), _ func()) (string, string, bool) {
	g.mu.Lock()
	g.texts = append(g.texts, text)
	g.mu.Unlock()
	return "", "", true
}

func (g *countingGateway) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.texts))
	copy(out, g.texts)
	return out
}

func makeMessageEvent(seq int64, content string) Event {
	payload, _ := json.Marshal(messagePayload{UserID: "u_other", Content: content})
	return Event{Seq: seq, Type: "message.created", Payload: payload}
}

func TestProcessEventBatch_CapsToMostRecentBacklog(t *testing.T) {
	gw := &countingGateway{}
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.maxCatchUpBacklog = 5

	const total = 8
	events := make([]Event, total)
	for i := 0; i < total; i++ {
		events[i] = makeMessageEvent(int64(i+1), fmt.Sprintf("msg-%d", i))
	}

	afterSeq := bot.processEventBatch(context.Background(), "conv-1", events, 0)

	if afterSeq != int64(total) {
		t.Errorf("afterSeq = %d, want %d (advances past skipped events too)", afterSeq, total)
	}

	texts := gw.recorded()
	if len(texts) != 5 {
		t.Fatalf("gateway.Handle called %d times, want 5", len(texts))
	}
	// The oldest 3 (msg-0..msg-2) should be skipped; the most recent 5 processed.
	want := []string{"msg-3", "msg-4", "msg-5", "msg-6", "msg-7"}
	for i, w := range want {
		if texts[i] != w {
			t.Errorf("texts[%d] = %q, want %q", i, texts[i], w)
		}
	}
}

func TestProcessEventBatch_UnderCap_ProcessesAll(t *testing.T) {
	gw := &countingGateway{}
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.maxCatchUpBacklog = 100

	events := []Event{makeMessageEvent(1, "a"), makeMessageEvent(2, "b"), makeMessageEvent(3, "c")}
	afterSeq := bot.processEventBatch(context.Background(), "conv-1", events, 0)

	if afterSeq != 3 {
		t.Errorf("afterSeq = %d, want 3", afterSeq)
	}
	if texts := gw.recorded(); len(texts) != 3 {
		t.Errorf("gateway.Handle called %d times, want 3, got %v", len(texts), texts)
	}
}

func TestProcessEventBatch_AlreadyProcessedEventsSkipped(t *testing.T) {
	gw := &countingGateway{}
	bot := newTestBot(t, &trackingStarter{}, gw)

	events := []Event{makeMessageEvent(1, "a"), makeMessageEvent(2, "b")}
	afterSeq := bot.processEventBatch(context.Background(), "conv-1", events, 2)

	if afterSeq != 2 {
		t.Errorf("afterSeq = %d, want 2 (unchanged)", afterSeq)
	}
	if texts := gw.recorded(); len(texts) != 0 {
		t.Errorf("expected no events processed (all seq <= afterSeq), got %v", texts)
	}
}

func TestProcessEventBatch_Empty(t *testing.T) {
	bot := newTestBot(t, &trackingStarter{}, &countingGateway{})
	if got := bot.processEventBatch(context.Background(), "conv-1", nil, 5); got != 5 {
		t.Errorf("processEventBatch(nil) = %d, want 5 (unchanged)", got)
	}
}

func TestConsumeSSEStream_ChannelClosed_ProcessesBurstAndReturns(t *testing.T) {
	gw := &countingGateway{}
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.maxCatchUpBacklog = 2

	ch := make(chan Event, 3)
	ch <- makeMessageEvent(1, "a")
	ch <- makeMessageEvent(2, "b")
	ch <- makeMessageEvent(3, "c")
	close(ch)

	afterSeq, received := bot.consumeSSEStream(context.Background(), "conv-1", ch, 0)

	if received != 3 {
		t.Errorf("received = %d, want 3", received)
	}
	if afterSeq != 3 {
		t.Errorf("afterSeq = %d, want 3", afterSeq)
	}
	texts := gw.recorded()
	if len(texts) != 2 {
		t.Fatalf("gateway.Handle called %d times, want 2 (capped), got %v", len(texts), texts)
	}
	if texts[0] != "b" || texts[1] != "c" {
		t.Errorf("unexpected processed order: %v, want [b c]", texts)
	}
}

func TestConsumeSSEStream_IdleTimeoutEndsBurstPhase(t *testing.T) {
	gw := &countingGateway{}
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.maxCatchUpBacklog = 100

	ch := make(chan Event, 2)
	ch <- makeMessageEvent(1, "a")
	ch <- makeMessageEvent(2, "b")
	// Don't close yet — let the idle timer end the burst phase on its own,
	// then close so phase 2's live-event loop exits.
	go func() {
		time.Sleep(backlogIdleWindow + 200*time.Millisecond)
		close(ch)
	}()

	afterSeq, received := bot.consumeSSEStream(context.Background(), "conv-1", ch, 0)

	if received != 2 {
		t.Errorf("received = %d, want 2", received)
	}
	if afterSeq != 2 {
		t.Errorf("afterSeq = %d, want 2", afterSeq)
	}
	if texts := gw.recorded(); len(texts) != 2 {
		t.Errorf("gateway.Handle called %d times, want 2, got %v", len(texts), texts)
	}
}
