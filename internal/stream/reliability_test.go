package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

func botAgainst(t *testing.T, h http.HandlerFunc) *Bot {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	mgr := thread.NewManager("")
	t.Cleanup(mgr.Stop)
	b := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t"}, "", &trackingStarter{}, mgr, nil, nil)
	b.botUserID = testBotUserID
	b.stateDir = t.TempDir()
	return b
}

// A first catch-up that could not read the history is not "caught up":
// saving its 0 would treat the channel's whole history as new messages.
func TestCatchUp_FailureIsNotCaughtUp(t *testing.T) {
	b := botAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	if _, caught := b.catchUpSeq(context.Background(), "c_1"); caught {
		t.Fatal("a failed catch-up was reported as caught up")
	}
}

// A listener that stops on its own (the server answered 404 for a passing
// error) is forgotten, so the next channel sync starts it again.
func TestListener_RestartableAfterItStops(t *testing.T) {
	var streams atomic.Int32
	b := botAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		streams.Add(1)
		http.NotFound(w, r)
	})
	os.WriteFile(b.cursorPath("c_1"), []byte("5"), 0o600) // resumes, straight to the live stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !b.startListener(ctx, "c_1") {
		t.Fatal("first start refused")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.listenersMu.Lock()
		_, still := b.listeners["c_1"]
		b.listenersMu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a listener that stopped on a 404 is still registered: the channel stays silent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !b.startListener(ctx, "c_1") {
		t.Fatal("the channel could not be listened to again")
	}
	cancel()
	b.wg.Wait()
}
