package stream

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThreadDispatcher_OrderedWithinThread(t *testing.T) {
	var wg sync.WaitGroup
	d := newThreadDispatcher(4, &wg)
	var mu sync.Mutex
	var got []int
	for i := 0; i < 20; i++ {
		i := i
		d.Dispatch("m_A", func() {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}
	wg.Wait()
	for i, v := range got {
		if v != i {
			t.Fatalf("thread handled out of order: %v", got)
		}
	}
	if len(got) != 20 {
		t.Fatalf("handled %d of 20", len(got))
	}
}

// A thread blocked in a slow handler must not hold up another thread.
func TestThreadDispatcher_ThreadsDoNotBlockEachOther(t *testing.T) {
	var wg sync.WaitGroup
	d := newThreadDispatcher(4, &wg)
	release := make(chan struct{})
	bDone := make(chan struct{})
	d.Dispatch("m_A", func() { <-release })
	d.Dispatch("m_B", func() { close(bDone) })
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("thread B waited behind thread A")
	}
	close(release)
	wg.Wait()
}

func TestThreadDispatcher_BoundsConcurrentHandlers(t *testing.T) {
	var wg sync.WaitGroup
	d := newThreadDispatcher(2, &wg)
	var running, peak atomic.Int32
	release := make(chan struct{})
	for _, key := range []string{"m_A", "m_B", "m_C", "m_D"} {
		d.Dispatch(key, func() {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
		})
	}
	time.Sleep(100 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrent handlers = %d, want 2", p)
	}
	close(release)
	wg.Wait()
}

func TestCursorTracker_StaysBehindUnfinishedMessages(t *testing.T) {
	var saved []int64
	tr := newCursorTracker(4, func(s int64) { saved = append(saved, s) })

	tr.begin(5)
	tr.begin(6)
	tr.advance(7) // non-message event after two in-flight messages
	if s := tr.safe(); s != 4 {
		t.Fatalf("safe = %d, want 4 while 5 is in flight", s)
	}
	tr.done(6)
	if s := tr.safe(); s != 4 {
		t.Fatalf("safe = %d, want 4: 5 is still in flight", s)
	}
	tr.done(5)
	if s := tr.safe(); s != 7 {
		t.Fatalf("safe = %d, want 7 once everything is handled", s)
	}
	if len(saved) != 1 || saved[0] != 7 {
		t.Fatalf("saved = %v, want [7] (never persist past an unfinished message)", saved)
	}
}

// blockingGateway blocks on messages whose text is "slow" until released,
// and records every text it handles.
type blockingGateway struct {
	release chan struct{}
	mu      sync.Mutex
	texts   []string
}

func (g *blockingGateway) Handle(text string, _ func(string), _ func()) (string, string, bool) {
	if text == "slow" {
		<-g.release
	}
	g.mu.Lock()
	g.texts = append(g.texts, text)
	g.mu.Unlock()
	return "", "", true
}

func (g *blockingGateway) handled() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.texts...)
}

// End to end through the bot: a slow message in thread A doesn't delay
// thread B, and the saved cursor waits for A before moving past it.
func TestStreamBot_ThreadsHandledInParallel(t *testing.T) {
	gw := &blockingGateway{release: make(chan struct{})}
	bot := newTestBot(t, &trackingStarter{}, gw)
	bot.stateDir = t.TempDir()

	msg := func(seq int64, id, content string) Event {
		p, _ := json.Marshal(messagePayload{MessageID: id, UserID: "u_human", Content: content})
		return Event{Seq: seq, Type: "message.created", Payload: p}
	}
	events := []Event{msg(1, "m_A", "slow"), msg(2, "m_B", "fast")}
	if got := bot.processEventBatch(context.Background(), "c_1", events, 0); got != 2 {
		t.Fatalf("processEventBatch returned %d, want 2 (both dispatched)", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(gw.handled()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := gw.handled(); len(got) != 1 || got[0] != "fast" {
		t.Fatalf("while thread A is busy, handled = %v, want [fast]", got)
	}
	if seq, _ := bot.loadCursor("c_1"); seq != 0 {
		t.Fatalf("cursor persisted at %d while seq 1 is unfinished, want 0", seq)
	}

	close(gw.release)
	bot.wg.Wait()
	if seq, _ := bot.loadCursor("c_1"); seq != 2 {
		t.Fatalf("cursor = %d after both handled, want 2", seq)
	}
}
