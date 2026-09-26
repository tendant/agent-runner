package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// threadStarter keeps every session running and records starts and steers.
type threadStarter struct {
	mu     sync.Mutex
	starts []string // thread keys passed to StartAgent
	steers []string // "sessionID:text"
}

func (s *threadStarter) StartAgent(_, _, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, key)
	return "s-" + key, nil
}

func (s *threadStarter) GetAgentSession(id string) (*agent.Session, bool) {
	return &agent.Session{ID: id, Status: agent.SessionStatusRunning}, true
}

func (s *threadStarter) Steer(sessionID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steers = append(s.steers, sessionID+":"+text)
	return nil
}

func messageEvent(t *testing.T, seq int64, messageID, threadID, content string) Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{
		"message_id": messageID, "thread_id": threadID, "user_id": "u_human", "content": content,
	})
	return Event{Seq: seq, Type: "message.created", Payload: payload}
}

// Each agent-stream thread gets its own state machine: a second thread root
// starts a second session instead of steering the first, and a reply steers
// only its own thread's session.
func TestStreamBot_ThreadsKeepIndependentState(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	threads := thread.NewManager("")
	defer threads.Stop()
	starter := &threadStarter{}
	bot := New(config.StreamConfig{ServerURL: srv.URL, BotToken: "test-token", ChannelIDs: []string{"c_1"}},
		"", starter, threads, nil, fakeGateway{})

	ctx := context.Background()
	bot.handleMessageEvent(ctx, "c_1", messageEvent(t, 1, "m_A", "", "build the site"))
	bot.handleMessageEvent(ctx, "c_1", messageEvent(t, 2, "m_B", "", "fix the login bug"))
	bot.handleMessageEvent(ctx, "c_1", messageEvent(t, 3, "m_A2", "m_A", "use blue"))

	starter.mu.Lock()
	starts, steers := starter.starts, starter.steers
	starter.mu.Unlock()
	if strings.Join(starts, ",") != "m_A,m_B" {
		t.Fatalf("expected one session per thread root, got starts %v", starts)
	}
	if strings.Join(steers, ",") != "s-m_A:use blue" {
		t.Fatalf("reply should steer only thread A's session, got %v", steers)
	}

	for key, want := range map[string]thread.State{"m_A": thread.StateExecuting, "m_B": thread.StateExecuting} {
		th, ok := threads.Get(key)
		if !ok || th.GetState() != want {
			t.Fatalf("thread %s: expected state %s", key, want)
		}
	}
	if _, ok := threads.Get("c_1"); ok {
		t.Fatal("no state should be kept per channel")
	}

	// Replies go to each thread, never the bare channel.
	mu.Lock()
	defer mu.Unlock()
	var sawA, sawB bool
	for _, p := range paths {
		switch {
		case strings.HasPrefix(p, "/v2/threads/m_A/"):
			sawA = true
		case strings.HasPrefix(p, "/v2/threads/m_B/"):
			sawB = true
		case strings.HasPrefix(p, "/v2/channels/"):
			t.Errorf("unexpected channel-level send: %s", p)
		}
	}
	if !sawA || !sawB {
		t.Fatalf("expected sends to both threads, got %v", paths)
	}
}

func TestThreadKey(t *testing.T) {
	cases := []struct {
		msg  messagePayload
		want string
	}{
		{messagePayload{MessageID: "m_root"}, "m_root"},
		{messagePayload{MessageID: "m_reply", ThreadID: "m_root"}, "m_root"},
		{messagePayload{}, "c_1"}, // server without threads
	}
	for _, tc := range cases {
		if got := threadKey("c_1", tc.msg); got != tc.want {
			t.Errorf("threadKey(%+v) = %s, want %s", tc.msg, got, tc.want)
		}
	}
}
