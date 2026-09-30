package stream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// takeoverServer is a fake agent-stream that records X-Bot-Instance and,
// once replaced is set, answers the way the server does after another
// process took over the bot.
type takeoverServer struct {
	mu        sync.Mutex
	instances map[string]bool
	sseReplay bool // end SSE streams with bot.replaced
	reject    bool // answer 409 bot_instance_replaced
}

func (f *takeoverServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.instances == nil {
		f.instances = map[string]bool{}
	}
	f.instances[r.Header.Get("X-Bot-Instance")] = true
	sse, reject := f.sseReplay, f.reject
	f.mu.Unlock()
	switch {
	case reject:
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"taken over","code":"bot_instance_replaced"}`)
	case r.URL.Path == "/v2/channels":
		fmt.Fprint(w, `[{"channel_id":"c_1"}]`)
	case strings.HasSuffix(r.URL.Path, "/events/stream"):
		w.Header().Set("Content-Type", "text/event-stream")
		if sse {
			fmt.Fprint(w, "event: bot.replaced\ndata: {\"code\":\"bot_instance_replaced\"}\n\n")
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	default:
		fmt.Fprint(w, "[]")
	}
}

func startTakeoverBot(t *testing.T, f *takeoverServer) (*Bot, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	threadMgr := thread.NewManager("")
	t.Cleanup(threadMgr.Stop)
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t", ChannelIDs: []string{"c_1"}}, "", &trackingStarter{}, threadMgr, nil, nil)
	bot.botUserID = testBotUserID
	bot.stateDir = t.TempDir()
	bot.saveCursor("c_1", 0) // skip the initial catch-up scan
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := bot.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return bot, ctx
}

func waitStopped(t *testing.T, bot *Bot) {
	t.Helper()
	done := make(chan struct{})
	go func() { bot.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream bot kept running after it was replaced")
	}
}

// Every request names this process, and a bot.replaced event stops the
// stream bot instead of reconnecting.
func TestStreamBot_StopsWhenReplacedOnStream(t *testing.T) {
	f := &takeoverServer{sseReplay: true}
	bot, _ := startTakeoverBot(t, f)
	waitStopped(t, bot)
	if !bot.client.replaced.Load() {
		t.Fatal("bot.replaced not recorded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.instances) != 1 || f.instances[""] {
		t.Fatalf("X-Bot-Instance values = %v, want one non-empty ID", f.instances)
	}
}

// A 409 bot_instance_replaced on any request stops it too.
func TestStreamBot_StopsWhenRequestsAreRefused(t *testing.T) {
	f := &takeoverServer{reject: true}
	bot, _ := startTakeoverBot(t, f)
	waitStopped(t, bot)
	if !bot.client.replaced.Load() {
		t.Fatal("409 bot_instance_replaced not recorded")
	}
}
