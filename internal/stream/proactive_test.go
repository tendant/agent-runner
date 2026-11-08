package stream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newProactiveTestBot(t *testing.T) (*Bot, *trackingStarter, *thread.Manager, *[]string, *sync.Mutex) {
	t.Helper()
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
	return bot, starter, threadMgr, &posted, &mu
}

// A yes in a proposal's thread goes to the proposal, not to the agent.
func TestStreamBot_ProposalThreadReply(t *testing.T) {
	bot, starter, _, posted, mu := newProactiveTestBot(t)
	key, err := bot.OpenProposalThread(context.Background(), "c_1", "**Proposal p-1: lint first**")
	if err != nil || key != "m_root" {
		t.Fatalf("key %q err %v", key, err)
	}
	var got []string
	bot.SetThreadReplies(func(thread, user string, admin bool, text string) (string, bool) {
		got = append(got, thread+"|"+user+"|"+text)
		if thread == "m_root" && text == "yes" {
			return "ok applied proposal p-1", true
		}
		return "", false
	})
	p, _ := json.Marshal(messagePayload{MessageID: "m_2", ThreadID: "m_root", UserID: "u_human", SenderKind: "human", Content: "yes", Addressees: []string{testBotUserID}})
	bot.processEventBatch(context.Background(), "c_1", []Event{{Seq: 1, Type: "message.created", Payload: p}}, 0)
	bot.wg.Wait()
	if len(got) != 1 || got[0] != "m_root|u_human|yes" {
		t.Fatalf("hook calls %v", got)
	}
	if starter.called.Load() {
		t.Fatal("a proposal's yes started the agent")
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, r := range *posted {
		if strings.Contains(r, "ok applied proposal p-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reply not posted: %v", *posted)
	}
}

// The retrospective runs at once in the thread it opens.
func TestStreamBot_StartProactiveRun(t *testing.T) {
	bot, starter, threadMgr, _, _ := newProactiveTestBot(t)
	id, thread, err := bot.StartProactiveRun(context.Background(), "c_1", "**Weekly retrospective**", "[Weekly retrospective] review the week")
	if err != nil || id == "" || thread != "m_root" {
		t.Fatalf("id %q thread %q err %v", id, thread, err)
	}
	bot.wg.Wait()
	if !starter.called.Load() {
		t.Fatal("the run did not start")
	}
	if conv := threadMgr.GetOrCreate("m_root"); conv.Scope != "c_1" {
		t.Fatalf("scope %q", conv.Scope)
	}
}
