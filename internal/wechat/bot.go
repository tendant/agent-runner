package wechat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter interface {
	StartAgent(message string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// Bot is a WeChat bot that bridges messages to the agent runner via the
// Tencent iLink API.
type Bot struct {
	client  *Client
	starter AgentStarter

	convManager *conversation.Manager
	analyzer    *conversation.Analyzer

	// contextTokens maps fromUserID → most-recently-received context_token.
	// The token must be echoed in replies so the iLink server can route them.
	tokenMu   sync.Mutex
	ctxTokens map[string]string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new WeChat bot. Returns nil if no token is configured.
// The actual connection is deferred to Start().
func New(cfg config.WeChatConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer) *Bot {
	if cfg.Token == "" {
		return nil
	}
	return &Bot{
		client:      NewClient(cfg.BaseURL, cfg.Token),
		starter:     starter,
		convManager: convMgr,
		analyzer:    analyzer,
		ctxTokens:   make(map[string]string),
	}
}

// Start begins long-polling the iLink API. Non-blocking.
func (b *Bot) Start(ctx context.Context) error {
	ctx, b.cancel = context.WithCancel(ctx)

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runLoop(ctx)
	}()

	slog.Info("wechat bot started")
	return nil
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	slog.Info("wechat bot stopped")
}

// runLoop continuously long-polls getupdates and dispatches inbound messages.
func (b *Bot) runLoop(ctx context.Context) {
	var buf string // sync cursor, updated after each poll

	for {
		if ctx.Err() != nil {
			return
		}

		resp, err := b.client.GetUpdates(ctx, buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("wechat: getupdates error", "error", err)
			// Brief pause before retrying to avoid tight error loops.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		if resp.GetUpdatesBuf != "" {
			buf = resp.GetUpdatesBuf
		}

		for _, msg := range resp.Msgs {
			b.storeContextToken(msg.FromUserID, msg.ContextToken)
			b.handleMessage(msg)
		}
	}
}

// storeContextToken persists the latest context_token for a user.
func (b *Bot) storeContextToken(userID, token string) {
	if userID == "" || token == "" {
		return
	}
	b.tokenMu.Lock()
	b.ctxTokens[userID] = token
	b.tokenMu.Unlock()
}

func (b *Bot) getContextToken(userID string) string {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	return b.ctxTokens[userID]
}

// handleMessage processes a single inbound WeChat message.
func (b *Bot) handleMessage(msg WeixinMessage) {
	if msg.MessageType != MessageTypeText {
		return // V1: text only
	}

	text := extractText(msg)
	if text == "" {
		return
	}

	userID := msg.FromUserID
	chatID := userID // use WeChat user ID as conversation key

	// Handle /cancel command
	if strings.EqualFold(strings.TrimSpace(text), "/cancel") {
		b.convManager.Complete(chatID)
		b.sendText(context.Background(), userID, "Conversation cancelled. Send a new message to start over.")
		return
	}

	conv := b.convManager.GetOrCreate(chatID)
	conv.AddMessage("user", text)

	state := conv.GetState()

	if state == conversation.StateExecuting {
		b.sendText(context.Background(), userID, "Message queued — I'll process it after the current task finishes.")
		return
	}

	if state == conversation.StateConfirming {
		if isConfirmation(text) {
			b.handleConfirmation(userID, chatID, conv)
			return
		}
		if isDenial(text) {
			conv.SetState(conversation.StateGathering)
			conv.AddMessage("assistant", "OK, what would you like to change?")
			b.sendText(context.Background(), userID, "OK, what would you like to change?")
			return
		}
	}

	b.sendText(context.Background(), userID, "Thinking...")
	b.handleAnalysis(userID, chatID, conv)
}

// handleConfirmation starts the agent after the user confirms.
func (b *Bot) handleConfirmation(userID, chatID string, conv *conversation.Conversation) {
	b.sendText(context.Background(), userID, "Starting agent...")
	conv.SetState(conversation.StateExecuting)

	messages := conv.GetMessages()
	var currentMsg string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			currentMsg = messages[i].Content
			break
		}
	}
	message := currentMsg
	if history := conv.GetFormattedHistory(); history != "" {
		message = fmt.Sprintf("## Conversation History\n\n%s\n\n## Current Request\n\n%s", history, currentMsg)
	}

	sessionID, err := b.starter.StartAgent(message)
	if err != nil {
		conv.SetState(conversation.StateGathering)
		b.sendText(context.Background(), userID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	b.sendText(context.Background(), userID, fmt.Sprintf("Agent session started: %s", sessionID))

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(userID, sessionID)

		if session, ok := b.starter.GetAgentSession(sessionID); ok {
			for _, iter := range session.Iterations {
				if iter.Output != "" {
					conv.AddMessage("assistant", iter.Output)
				}
			}
			if len(session.OutputFiles) > 0 {
				var names []string
				for _, f := range session.OutputFiles {
					names = append(names, f.Name)
				}
				b.sendText(context.Background(), userID, fmt.Sprintf("Output files: %s", strings.Join(names, ", ")))
			}
		}

		if b.analyzer != nil && conv.NeedsCompaction() {
			b.summarizeConversation(conv)
		}

		if conv.ClearPendingInput() {
			conv.SetState(conversation.StateGathering)
			b.sendText(context.Background(), userID, "Processing queued messages...")
			b.handleAnalysis(userID, chatID, conv)
			return
		}

		b.convManager.Complete(chatID)
	}()
}

// handleAnalysis calls the analyzer to decide the next action.
func (b *Bot) handleAnalysis(userID, chatID string, conv *conversation.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := b.analyzer.Analyze(ctx, conv)
	if err != nil {
		slog.Error("wechat: analyzer error", "error", err)
		if ctx.Err() == context.DeadlineExceeded {
			b.sendText(context.Background(), userID, "Sorry, the request timed out. Please try again.")
		} else {
			b.sendText(context.Background(), userID, "Sorry, I had trouble understanding that. Could you rephrase?")
		}
		return
	}

	switch result.Action {
	case "execute":
		conv.AddMessage("assistant", result.Message)
		b.sendText(context.Background(), userID, result.Message)
		b.handleConfirmation(userID, chatID, conv)

	case "ask":
		conv.AddMessage("assistant", result.Message)
		b.sendText(context.Background(), userID, result.Message)

	case "plan":
		conv.SetPlan(result.Message)
		conv.AddMessage("assistant", result.Message)
		b.sendText(context.Background(), userID, result.Message+"\n\nProceed? (yes/no)")

	default:
		conv.AddMessage("assistant", result.Message)
		b.sendText(context.Background(), userID, result.Message)
	}
}

// pollAndReport polls the agent session every 5 seconds and sends incremental
// iteration updates to the user.
func (b *Bot) pollAndReport(userID, sessionID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	reported := 0

	for range ticker.C {
		session, exists := b.starter.GetAgentSession(sessionID)
		if !exists {
			b.sendText(context.Background(), userID, "Session not found.")
			return
		}

		for i := reported; i < len(session.Iterations); i++ {
			b.sendText(context.Background(), userID, formatIteration(session.Iterations[i]))
		}
		reported = len(session.Iterations)

		if session.Status == agent.SessionStatusCompleted || session.Status == agent.SessionStatusFailed {
			b.sendText(context.Background(), userID, formatFinalResult(session))
			return
		}
	}
}

// sendText sends a plain-text message to a WeChat user.
func (b *Bot) sendText(ctx context.Context, userID, text string) {
	token := b.getContextToken(userID)
	if err := b.client.SendMessage(ctx, userID, text, token); err != nil {
		slog.Error("wechat: failed to send message", "user_id", userID, "error", err)
	}
}

// summarizeConversation compacts old messages into a summary.
func (b *Bot) summarizeConversation(conv *conversation.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := conv.GetMessages()
	keepRecent := len(msgs) / 2
	if keepRecent < 4 {
		keepRecent = 4
	}
	toSummarize := msgs[:len(msgs)-keepRecent]
	if len(toSummarize) == 0 {
		return
	}
	summary, err := b.analyzer.Summarize(ctx, toSummarize)
	if err != nil {
		slog.Warn("wechat: conversation summarization failed", "error", err)
		return
	}
	conv.CompactWithSummary(summary, keepRecent)
	slog.Info("wechat: conversation compacted", "summary_len", len(summary), "kept_recent", keepRecent)
}

// extractText returns the plain text from the first text item in a message.
func extractText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == MessageTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

func formatIteration(iter agent.IterationResult) string {
	var sb strings.Builder
	switch iter.Status {
	case agent.IterationStatusSuccess:
		fmt.Fprintf(&sb, "Iteration %d: completed", iter.Iteration)
		if iter.Commit != "" {
			fmt.Fprintf(&sb, " (commit %s)", iter.Commit)
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
	if iter.Output != "" {
		output := iter.Output
		if len(output) > 3000 {
			output = output[:3000] + "\n... (truncated)"
		}
		fmt.Fprintf(&sb, "\n\n%s", output)
	}
	return sb.String()
}

func formatFinalResult(session *agent.Session) string {
	var sb strings.Builder
	if session.Status == agent.SessionStatusCompleted {
		fmt.Fprintf(&sb, "Session completed — %d iterations in %ds", len(session.Iterations), session.ElapsedSeconds)
	} else {
		fmt.Fprintf(&sb, "Session failed")
		if session.Error != "" {
			fmt.Fprintf(&sb, " — %s", session.Error)
		}
	}
	if len(session.OutputFiles) > 0 {
		fmt.Fprintf(&sb, "\n\n%d file(s) attached", len(session.OutputFiles))
	}
	return sb.String()
}

func isConfirmation(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "yes", "y", "ok", "sure", "proceed", "go", "do it", "confirm", "yep", "yeah":
		return true
	}
	return false
}

func isDenial(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "no", "n", "nope", "cancel", "stop", "nah", "nevermind", "never mind":
		return true
	}
	return false
}
