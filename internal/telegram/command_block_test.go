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

// fakeGateway handles "/set X Y", blocks other slash commands, and passes
// regular messages through — matching the behaviour of api.MessageGateway.
type fakeGateway struct{}

func (fakeGateway) Handle(text string, _ func(string), reset func()) (string, string, bool) {
	if strings.ToLower(strings.TrimSpace(text)) == "/cancel" {
		if reset != nil { reset() }
		return "Conversation cancelled. Send a new message to start over.", "", true
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(lower, "/set ") {
		return "ok KEY=VALUE", "", true
	}
	if strings.HasPrefix(lower, "/") {
		return "Unknown command. Type /help for available commands.", "", true
	}
	return "", "", false
}

func newTestBot(t *testing.T, starter AgentStarter, gw Gateway) *Bot {
	t.Helper()
	convMgr := conversation.NewManager("")
	t.Cleanup(convMgr.Stop)
	// api is nil — send() is nil-safe so no panics during tests.
	return New(config.TelegramConfig{BotToken: "test", ChatID: 42}, starter, convMgr, nil, t.TempDir(), gw)
}

func tgMsg(chatID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: chatID},
		Text: text,
	}
}

// tgCmd builds a Telegram command message (with bot_command entity so
// IsCommand() returns true and Command() returns the bare command name).
func tgCmd(chatID int64, cmd, botName string) *tgbotapi.Message {
	full := "/" + cmd
	if botName != "" {
		full = "/" + cmd + "@" + botName
	}
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: chatID},
		Text: full,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: len(full)},
		},
	}
}

func TestTelegramBot_KnownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeGateway{})

	bot.handleMessage(tgMsg(42, "/set AGENT_MODEL gpt-4o"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called when a known command is handled")
	}
}

func TestTelegramBot_UnknownCommand_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeGateway{})

	bot.handleMessage(tgMsg(42, "/unknown-command"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called for an unknown slash command")
	}
}

func TestTelegramBot_RegularMessage_StartsAgent(t *testing.T) {
	starter := &trackingStarter{}
	// No analyzer → direct execution mode.
	bot := newTestBot(t, starter, fakeGateway{})

	bot.handleMessage(tgMsg(42, "please write hello.txt"))

	if !starter.called.Load() {
		t.Error("StartAgent should be called for a regular (non-command) message")
	}
}

func TestTelegramBot_GroupCancelWithBotSuffix_ResetsConversation(t *testing.T) {
	starter := &trackingStarter{}
	convMgr := conversation.NewManager("")
	t.Cleanup(convMgr.Stop)
	bot := New(config.TelegramConfig{BotToken: "test", ChatID: 42}, starter, convMgr, nil, t.TempDir(), fakeGateway{})

	// Seed a conversation so we can verify it gets reset.
	conv := convMgr.GetOrCreate("42")
	conv.AddMessage("user", "do something")

	// Group-chat style: Telegram delivers "/cancel@AgentRunnerBot".
	bot.handleMessage(tgCmd(42, "cancel", "AgentRunnerBot"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called for /cancel")
	}
	// Conversation should be completed (reset).
	if convMgr.GetOrCreate("42").GetState() != conversation.StateGathering {
		t.Error("expected conversation to be reset after /cancel@BotName")
	}
}

func TestTelegramBot_UnauthorizedChat_DoesNotStartAgent(t *testing.T) {
	starter := &trackingStarter{}
	bot := newTestBot(t, starter, fakeGateway{})

	// Send from a different chat ID than configured (42).
	bot.handleMessage(tgMsg(99, "do something"))

	if starter.called.Load() {
		t.Error("StartAgent should not be called from an unauthorized chat")
	}
}
