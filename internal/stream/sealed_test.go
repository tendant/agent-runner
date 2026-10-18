package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// A sealed secret command is read once from agent-stream and handed only to
// the command gateway: it never enters the thread's history.
func TestStreamBot_SealedCommand(t *testing.T) {
	const original = "/secret set GITEE_TOKEN tok-1234567890 Gitee"
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/messages/m_1/sealed" && reads.Add(1) == 1 {
			json.NewEncoder(w).Encode(map[string]string{"content": original})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sealed") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	gw := &blockingGateway{release: make(chan struct{})}
	close(gw.release)
	mgr := thread.NewManager("")
	t.Cleanup(mgr.Stop)
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "t"}, "", &trackingStarter{}, mgr, nil, gw)
	bot.botUserID = testBotUserID
	bot.stateDir = t.TempDir()

	send := func(seq int64, id string) {
		p, _ := json.Marshal(messagePayload{MessageID: id, UserID: "u_h", SenderKind: "human", Content: "/secret set GITEE_TOKEN ••••••", Sealed: true, Addressees: []string{testBotUserID}})
		bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: seq, Type: "message.created", Payload: p}}, seq-1)
		bot.wg.Wait()
	}
	send(1, "m_1")
	if got := gw.handled(); len(got) != 1 || got[0] != original {
		t.Fatalf("gateway got %q, want the original command", got)
	}
	if conv, ok := mgr.Get("m_1"); ok && len(conv.GetMessages()) > 0 {
		t.Fatalf("the command entered the thread's history: %v", conv.GetMessages())
	}

	// Already read (or expired): nothing reaches the gateway.
	send(2, "m_1")
	if got := gw.handled(); len(got) != 1 {
		t.Fatalf("a second delivery reached the gateway: %q", got)
	}
}
