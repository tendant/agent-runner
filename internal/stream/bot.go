package stream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
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

// Bot bridges agent-stream conversations to the agent runner.
type Bot struct {
	client      *Client
	starter     AgentStarter
	convManager *conversation.Manager
	analyzer    *conversation.Analyzer
	convIDs     []string
	botUserID   string
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// New creates a new stream bot. Returns nil if ServerURL or BotToken is empty.
func New(cfg config.StreamConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer) *Bot {
	if cfg.ServerURL == "" || cfg.BotToken == "" {
		return nil
	}

	return &Bot{
		client:      NewClient(cfg.ServerURL, cfg.BotToken),
		starter:     starter,
		convManager: convMgr,
		analyzer:    analyzer,
		convIDs:     cfg.ConversationIDs,
		botUserID:   extractBotUserID(cfg.BotToken),
	}
}

// Start begins listening on all configured conversations. Non-blocking.
func (b *Bot) Start(ctx context.Context) error {
	if len(b.convIDs) == 0 {
		log.Println("Stream bot: no conversation IDs configured, not starting")
		return nil
	}

	ctx, b.cancel = context.WithCancel(ctx)

	for _, convID := range b.convIDs {
		convID := convID // capture for goroutine
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.listenConversation(ctx, convID)
		}()
	}

	log.Printf("Stream bot started, listening to %d conversations", len(b.convIDs))
	return nil
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	log.Println("Stream bot stopped")
}

// listenConversation connects to SSE for a single conversation and processes events.
func (b *Bot) listenConversation(ctx context.Context, convID string) {
	var afterSeq int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		events, err := b.client.StreamEvents(ctx, convID, afterSeq)
		if err != nil {
			log.Printf("Stream bot: SSE connect error for %s: %v", convID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		for event := range events {
			afterSeq = event.Seq
			if event.Type == "message.created" {
				b.handleMessageEvent(ctx, convID, event)
			}
		}

		// Channel closed — reconnect after delay
		log.Printf("Stream bot: SSE connection closed for %s, reconnecting...", convID)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// messagePayload is the shape of a message.created event payload.
type messagePayload struct {
	UserID  string `json:"user_id"`
	Content string `json:"content"`
}

func (b *Bot) handleMessageEvent(ctx context.Context, convID string, event Event) {
	var msg messagePayload
	if err := json.Unmarshal(event.Payload, &msg); err != nil {
		log.Printf("Stream bot: failed to parse message payload: %v", err)
		return
	}

	// Ignore own messages
	if msg.UserID == b.botUserID {
		return
	}

	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return
	}

	b.handleMessage(ctx, convID, text)
}

func (b *Bot) handleMessage(ctx context.Context, convID, text string) {
	// Handle /cancel command
	if text == "/cancel" {
		b.convManager.Complete(convID)
		b.emitFinal(ctx, convID, "Conversation cancelled. Send a new message to start over.")
		return
	}

	conv := b.convManager.GetOrCreate(convID)
	conv.AddMessage("user", text)

	state := conv.GetState()

	if state == conversation.StateExecuting {
		b.emitFinal(ctx, convID, "Agent is currently running. Please wait for it to finish.")
		return
	}

	if state == conversation.StateConfirming {
		if isConfirmation(text) {
			b.handleConfirmation(ctx, convID, conv)
			return
		}
		if isDenial(text) {
			conv.SetState(conversation.StateGathering)
			resp := "OK, what would you like to change?"
			conv.AddMessage("assistant", resp)
			b.emitFinal(ctx, convID, resp)
			return
		}
	}

	b.emitThinking(ctx, convID, "Thinking...")
	b.handleAnalysis(ctx, convID, conv)
}

func (b *Bot) handleConfirmation(ctx context.Context, convID string, conv *conversation.Conversation) {
	b.emitThinking(ctx, convID, "Starting agent...")
	conv.SetState(conversation.StateExecuting)

	message := conv.GetUserMessage()

	sessionID, err := b.starter.StartAgent(message)
	if err != nil {
		conv.SetState(conversation.StateGathering)
		b.emitFinal(ctx, convID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	b.emitDelta(ctx, convID, fmt.Sprintf("Agent session started: %s\n", sessionID))

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(convID, sessionID)
		b.convManager.Complete(convID)
	}()
}

func (b *Bot) handleAnalysis(ctx context.Context, convID string, conv *conversation.Conversation) {
	analysisCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := b.analyzer.Analyze(analysisCtx, conv)
	if err != nil {
		log.Printf("Stream bot: analyzer error: %v", err)
		if analysisCtx.Err() == context.DeadlineExceeded {
			b.emitFinal(ctx, convID, "Sorry, the request timed out. Please try again.")
		} else {
			b.emitFinal(ctx, convID, "Sorry, I had trouble understanding that. Could you rephrase?")
		}
		return
	}

	switch result.Action {
	case "execute":
		conv.AddMessage("assistant", result.Message)
		b.emitDelta(ctx, convID, result.Message+"\n")
		b.handleConfirmation(ctx, convID, conv)

	case "ask":
		conv.AddMessage("assistant", result.Message)
		b.emitFinal(ctx, convID, result.Message)

	case "plan":
		conv.SetPlan(result.Message)
		conv.AddMessage("assistant", result.Message)
		b.emitFinal(ctx, convID, result.Message+"\n\nProceed? (yes/no)")

	default:
		conv.AddMessage("assistant", result.Message)
		b.emitFinal(ctx, convID, result.Message)
	}
}

func (b *Bot) pollAndReport(convID, sessionID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	ctx := context.Background()
	reported := 0

	for range ticker.C {
		session, exists := b.starter.GetAgentSession(sessionID)
		if !exists {
			b.emitFinal(ctx, convID, "Session not found.")
			return
		}

		for i := reported; i < len(session.Iterations); i++ {
			b.emitDelta(ctx, convID, formatIteration(session.Iterations[i])+"\n")
		}
		reported = len(session.Iterations)

		if session.Status == agent.SessionStatusCompleted || session.Status == agent.SessionStatusFailed {
			b.emitFinal(ctx, convID, formatFinalResult(session))
			return
		}
	}
}

// Event emission helpers

func (b *Bot) emitThinking(ctx context.Context, convID, msg string) {
	b.emit(ctx, convID, "status.thinking", map[string]string{"text": msg})
}

func (b *Bot) emitDelta(ctx context.Context, convID, text string) {
	b.emit(ctx, convID, "assistant.delta", map[string]string{"text": text})
}

func (b *Bot) emitFinal(ctx context.Context, convID, text string) {
	b.emit(ctx, convID, "assistant.final", map[string]string{"text": text})
}

func (b *Bot) emit(ctx context.Context, convID, eventType string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Stream bot: marshal error: %v", err)
		return
	}
	if err := b.client.EmitEvent(ctx, convID, eventType, data); err != nil {
		log.Printf("Stream bot: emit error (%s): %v", eventType, err)
	}
}

// Formatting helpers (mirrors telegram package)

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
		if len(output) > 4000 {
			output = output[:4000] + "\n... (truncated)"
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
	return sb.String()
}

// Helpers

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

// extractBotUserID extracts a user ID from a JWT token (base64-decoded middle segment).
// Falls back to empty string if parsing fails — own-message filtering will be skipped.
func extractBotUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}

	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	return claims.Sub
}
