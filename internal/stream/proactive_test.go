package stream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// A proactive proposal opens its own thread; a person's yes in it runs it.
func TestStreamBot_ProactiveThreadWaitsForYes(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted = append(posted, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v2/channels/c_1/messages" {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"accepted":true,"message_id":"m_root"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	threadMgr := thread.NewManager("")
	t.Cleanup(threadMgr.Stop)
	starter := &trackingStarter{}
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t", ChannelIDs: []string{"c_1"}}, "", starter, threadMgr, nil, fakeGateway{})
	bot.botUserID = testBotUserID
	bot.stateDir = t.TempDir()

	if err := bot.OpenProactiveThread(context.Background(), "c_1", "[Proactive check] report", "**Proactive check**\n\nrerun the job?\n\nProceed? (yes/no)", true); err != nil {
		t.Fatal(err)
	}
	conv := threadMgr.GetOrCreate("m_root")
	if conv.GetState() != thread.StateConfirming || conv.Scope != "c_1" {
		t.Fatalf("thread state %s scope %q", conv.GetState(), conv.Scope)
	}
	if starter.called.Load() {
		t.Fatal("a proposal started the agent before a yes")
	}

	p, _ := json.Marshal(messagePayload{MessageID: "m_2", ThreadID: "m_root", UserID: "u_human", SenderKind: "human", Content: "yes", Addressees: []string{testBotUserID}})
	bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: 1, Type: "message.created", Payload: p}}, 0)
	bot.wg.Wait()
	if !starter.called.Load() {
		t.Fatalf("yes did not run the proposal; requests: %v", posted)
	}
}

func TestPostThreadRootNeedsAChannel(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "t")
	if _, err := c.PostThreadRoot(context.Background(), "m_1", OutMessage{Content: "x"}); err == nil {
		t.Fatal("posted a thread root into a thread")
	}
}
