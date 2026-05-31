package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

// trackingStarter records whether StartAgent was called.
type trackingStarter struct {
	called atomic.Bool
}

func (s *trackingStarter) StartAgent(_, _ string) (string, error) {
	s.called.Store(true)
	return "session-123", nil
}

func (s *trackingStarter) GetAgentSession(id string) (*agent.Session, bool) {
	return &agent.Session{ID: id, Status: agent.SessionStatusCompleted}, true
}

// fakeCommander handles "/set X Y" and returns unhandled for everything else.
type fakeCommander struct{}

func (fakeCommander) Handle(text string, _ func(string)) (string, string, bool) {
	if strings.HasPrefix(strings.ToLower(text), "/set ") {
		return "ok KEY=VALUE", "", true
	}
	return "", "", false
}

// newTestBot creates a Bot wired to a throwaway HTTP server for emits.
func newTestBot(t *testing.T, starter AgentStarter, commander Commander) *Bot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config.StreamConfig{
		ServerURL:       srv.URL,
		BotToken:        "test-token",
		ConversationIDs: []string{"conv-1"},
	}
	convMgr := conversation.NewManager("")
	t.Cleanup(convMgr.Stop)

	return New(cfg, "", starter, convMgr, nil, commander)
}

func TestStreamBot_KnownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(context.Background(), "conv-1", "/set AGENT_MODEL gpt-4o")

	if starter.called.Load() {
		t.Error("StartAgent should not be called when a known command is handled")
	}
}

func TestStreamBot_UnknownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(context.Background(), "conv-1", "/unknown-command")

	if starter.called.Load() {
		t.Error("StartAgent should not be called for an unknown slash command")
	}
}

func TestStreamBot_UnknownCommand_ReturnsHelpHint(t *testing.T) {
	var replied string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the emitted payload to verify the reply.
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		replied += string(body[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.StreamConfig{
		ServerURL:       srv.URL,
		BotToken:        "test-token",
		ConversationIDs: []string{"conv-1"},
	}
	convMgr := conversation.NewManager("")
	defer convMgr.Stop()

	bot := New(cfg, "", &trackingStarter{}, convMgr, nil, fakeCommander{})
	bot.handleMessage(context.Background(), "conv-1", "/oops")

	if !strings.Contains(replied, "Unknown command") {
		t.Errorf("expected 'Unknown command' hint in reply, got: %s", replied)
	}
}

func TestStreamBot_RegularMessage_StartsAgent(t *testing.T) {
	starter := &trackingStarter{}
	// No analyzer → direct execution mode.
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(context.Background(), "conv-1", "please write hello.txt")

	if !starter.called.Load() {
		t.Error("StartAgent should be called for a regular (non-command) message")
	}
}
