package telegram

import (
	"strings"
	"sync/atomic"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

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

func newTestBot(t *testing.T, starter AgentStarter, commander Commander) *Bot {
	t.Helper()
	convMgr := conversation.NewManager("")
	t.Cleanup(convMgr.Stop)
	// api is nil — send() is nil-safe so no panics during tests.
	return New(config.TelegramConfig{BotToken: "test", ChatID: 42}, starter, convMgr, nil, t.TempDir(), commander)
}

func tgMsg(chatID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: chatID},
		Text: text,
	}
}

func TestTelegramBot_KnownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(tgMsg(42, "/set AGENT_MODEL gpt-4o"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called when a known command is handled")
	}
}

func TestTelegramBot_UnknownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(tgMsg(42, "/unknown-command"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called for an unknown slash command")
	}
}

func TestTelegramBot_RegularMessage_StartsAgent(t *testing.T) {
	starter := &trackingStarter{}
	// No analyzer → direct execution mode.
	bot := newTestBot(t, starter, fakeCommander{})

	bot.handleMessage(tgMsg(42, "please write hello.txt"))

	if !starter.called.Load() {
		t.Error("StartAgent should be called for a regular (non-command) message")
	}
}

func TestTelegramBot_UnauthorizedChat_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeCommander{})

	// Send from a different chat ID than configured (42).
	bot.handleMessage(tgMsg(99, "do something"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called from an unauthorized chat")
	}
}
