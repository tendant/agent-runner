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
	"github.com/agent-runner/agent-runner/internal/conversation"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter interface {
	StartAgent(message string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// ProjectLister returns the list of available project names.
type ProjectLister interface {
	ListAvailableProjects() []string
}

// Bot is a Telegram bot that bridges messages to the agent runner.
type Bot struct {
	token   string
	chatID  int64
	starter AgentStarter

	convManager *conversation.Manager
	analyzer    *conversation.Analyzer
	projLister  ProjectLister

	api    *tgbotapi.BotAPI
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Telegram bot. Returns nil if the token is empty.
// The actual API connection is deferred to Start().
func New(cfg config.TelegramConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer, projLister ProjectLister) *Bot {
	if cfg.BotToken == "" {
		return nil
	}

	return &Bot{
		token:       cfg.BotToken,
		chatID:      cfg.ChatID,
		starter:     starter,
		convManager: convMgr,
		analyzer:    analyzer,
		projLister:  projLister,
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
		b.send(msg.Chat.ID, "Agent Runner bot ready. Send a message to start a conversation.")
		return
	}

	// Handle /cancel command — reset conversation
	if msg.IsCommand() && msg.Command() == "cancel" {
		b.convManager.Complete(msg.Chat.ID)
		b.send(msg.Chat.ID, "Conversation cancelled. Send a new message to start over.")
		return
	}

	text := msg.Text
	if text == "" {
		return
	}

	chatID := msg.Chat.ID

	// Get or create conversation
	conv := b.convManager.GetOrCreate(chatID)
	conv.AddMessage("user", text)

	state := conv.GetState()

	// If currently executing, tell the user to wait
	if state == conversation.StateExecuting {
		b.send(chatID, "Agent is currently running. Please wait for it to finish.")
		return
	}

	// If confirming, check for yes/no
	if state == conversation.StateConfirming {
		if isConfirmation(text) {
			b.handleConfirmation(chatID, conv)
			return
		}
		if isDenial(text) {
			conv.SetState(conversation.StateGathering)
			conv.AddMessage("assistant", "OK, what would you like to change?")
			b.send(chatID, "OK, what would you like to change?")
			return
		}
		// Not a clear yes/no — treat as continued conversation
	}

	// Analyze conversation via Claude (slow — acknowledge first)
	b.send(chatID, "Thinking...")
	b.handleAnalysis(chatID, conv)
}

// handleConfirmation starts the agent after the user confirms the plan.
func (b *Bot) handleConfirmation(chatID int64, conv *conversation.Conversation) {
	b.send(chatID, "Starting agent...")
	conv.SetState(conversation.StateExecuting)

	// Build the enriched message from the plan + conversation context
	project := conv.GetProject()
	plan := conv.GetPlan()

	// Include @project tag so the starter routes correctly
	enrichedMessage := plan
	if project != "" {
		enrichedMessage = fmt.Sprintf("@%s %s", project, plan)
	}

	sessionID, err := b.starter.StartAgent(enrichedMessage)
	if err != nil {
		conv.SetState(conversation.StateGathering)
		b.send(chatID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	b.send(chatID, fmt.Sprintf("Agent session started: `%s`", sessionID))

	// Poll and report in background
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(chatID, sessionID)
		b.convManager.Complete(chatID)
	}()
}

// handleAnalysis calls the analyzer to decide the next action.
func (b *Bot) handleAnalysis(chatID int64, conv *conversation.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var projects []string
	if b.projLister != nil {
		projects = b.projLister.ListAvailableProjects()
	}

	result, err := b.analyzer.Analyze(ctx, conv, projects)
	if err != nil {
		log.Printf("Telegram: analyzer error: %v", err)
		b.send(chatID, "Sorry, I had trouble understanding that. Could you rephrase?")
		return
	}

	switch result.Action {
	case "ask":
		conv.AddMessage("assistant", result.Message)
		if result.Project != "" {
			conv.SetProject(result.Project)
		}
		b.send(chatID, result.Message)

	case "plan":
		if result.Project != "" {
			conv.SetProject(result.Project)
		}
		conv.SetPlan(result.Message)
		conv.AddMessage("assistant", result.Message)
		b.send(chatID, result.Message+"\n\nProceed? (yes/no)")

	default:
		// Unknown action — treat as "ask"
		conv.AddMessage("assistant", result.Message)
		b.send(chatID, result.Message)
	}
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
		fmt.Fprintf(&sb, "Iteration %d: completed", iter.Iteration)
		if iter.Commit != "" {
			fmt.Fprintf(&sb, " (commit `%s`)", iter.Commit)
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
		fmt.Fprintf(&sb, "Session completed — %d iterations in %ds", len(session.Iterations), session.ElapsedSeconds)
	} else {
		fmt.Fprintf(&sb, "Session failed")
		if session.Error != "" {
			fmt.Fprintf(&sb, " — %s", session.Error)
		}
	}
	return sb.String()
}

// isConfirmation checks if the message is a positive confirmation.
func isConfirmation(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "yes", "y", "ok", "sure", "proceed", "go", "do it", "confirm", "yep", "yeah":
		return true
	}
	return false
}

// isDenial checks if the message is a negative response.
func isDenial(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "no", "n", "nope", "cancel", "stop", "nah", "nevermind", "never mind":
		return true
	}
	return false
}
