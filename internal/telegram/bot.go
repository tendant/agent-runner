package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter interface {
	StartAgent(message string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// Bot is a Telegram bot that bridges messages to the agent runner.
type Bot struct {
	token   string
	chatID  int64
	starter AgentStarter

	api    *tgbotapi.BotAPI
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Telegram bot. Returns nil if the token is empty.
// The actual API connection is deferred to Start().
func New(cfg config.TelegramConfig, starter AgentStarter) *Bot {
	if cfg.BotToken == "" {
		return nil
	}

	return &Bot{
		token:   cfg.BotToken,
		chatID:  cfg.ChatID,
		starter: starter,
	}
}

// Start connects to the Telegram API and begins long-polling. Non-blocking.
// Returns an error if the bot token is invalid.
func (b *Bot) Start(ctx context.Context) error {
	api, err := tgbotapi.NewBotAPI(b.token)
	if err != nil {
		return fmt.Errorf("failed to connect to Telegram: %w", err)
	}
	b.api = api

	ctx, b.cancel = context.WithCancel(ctx)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30

	updates := b.api.GetUpdatesChan(u)

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				if update.Message == nil {
					continue
				}
				b.handleMessage(update.Message)
			}
		}
	}()

	log.Printf("Telegram bot started (@%s)", b.api.Self.UserName)
	return nil
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.api != nil {
		b.api.StopReceivingUpdates()
	}
	b.wg.Wait()
	log.Println("Telegram bot stopped")
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// Security: only respond to the configured chat ID
	if b.chatID != 0 && msg.Chat.ID != b.chatID {
		log.Printf("Telegram: ignoring message from unauthorized chat %d", msg.Chat.ID)
		return
	}

	// Handle /start command
	if msg.IsCommand() && msg.Command() == "start" {
		b.send(msg.Chat.ID, "Agent Runner bot ready. Send a message to start an agent session.")
		return
	}

	text := msg.Text
	if text == "" {
		return
	}

	// Start an agent session
	sessionID, err := b.starter.StartAgent(text)
	if err != nil {
		b.send(msg.Chat.ID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	b.send(msg.Chat.ID, fmt.Sprintf("Agent session started: `%s`", sessionID))

	// Poll and report in background
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(msg.Chat.ID, sessionID)
	}()
}

func (b *Bot) pollAndReport(chatID int64, sessionID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	reported := 0 // number of iterations already reported

	for range ticker.C {
		session, exists := b.starter.GetAgentSession(sessionID)
		if !exists {
			b.send(chatID, "Session not found.")
			return
		}

		// Report new iterations incrementally
		for i := reported; i < len(session.Iterations); i++ {
			b.send(chatID, FormatIteration(session.Iterations[i]))
		}
		reported = len(session.Iterations)

		// Check if session is done
		if session.Status == agent.SessionStatusCompleted || session.Status == agent.SessionStatusFailed {
			b.send(chatID, FormatFinalResult(session))
			return
		}
	}
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Telegram: failed to send message: %v", err)
	}
}

// FormatIteration formats a single iteration result for Telegram.
func FormatIteration(iter agent.IterationResult) string {
	var sb strings.Builder
	switch iter.Status {
	case agent.IterationStatusSuccess:
		fmt.Fprintf(&sb, "Iteration %d: committed `%s`", iter.Iteration, iter.Commit)
		if len(iter.ChangedFiles) > 0 {
			fmt.Fprintf(&sb, " (%d files)", len(iter.ChangedFiles))
		}
	case agent.IterationStatusNoChanges:
		fmt.Fprintf(&sb, "Iteration %d: no changes", iter.Iteration)
	case agent.IterationStatusValidation:
		fmt.Fprintf(&sb, "Iteration %d: validation failed — %s", iter.Iteration, iter.Error)
	case agent.IterationStatusError:
		fmt.Fprintf(&sb, "Iteration %d: error — %s", iter.Iteration, iter.Error)
	default:
		fmt.Fprintf(&sb, "Iteration %d: %s", iter.Iteration, iter.Status)
	}
	return sb.String()
}

// FormatFinalResult formats the final session summary for Telegram.
func FormatFinalResult(session *agent.Session) string {
	var sb strings.Builder
	if session.Status == agent.SessionStatusCompleted {
		fmt.Fprintf(&sb, "Session completed — %d commits in %ds", session.TotalCommits, session.ElapsedSeconds)
	} else {
		fmt.Fprintf(&sb, "Session failed")
		if session.Error != "" {
			fmt.Fprintf(&sb, " — %s", session.Error)
		}
	}
	return sb.String()
}
