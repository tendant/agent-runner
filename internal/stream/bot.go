package stream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
	"github.com/agent-runner/agent-runner/internal/wechat"
	qrcode "github.com/skip2/go-qrcode"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter interface {
	StartAgent(message, source string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// Commander handles chat configuration commands without requiring an LLM.
type Commander interface {
	Handle(text string) (string, bool)
}

// Bot bridges agent-stream conversations to the agent runner.
type Bot struct {
	client         *Client
	starter        AgentStarter
	commander      Commander
	convManager    *conversation.Manager
	analyzer       *conversation.Analyzer
	convIDs        []string
	botUserID      string
	wechatReloader  func(token, baseURL string) // called after a successful /wechat-login
	wechatBaseURL   string                     // iLink API base URL for the login flow
	wechatLoginMu   sync.Mutex                 // prevents concurrent /wechat-login flows
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

// SetWeChatReloader registers a callback that is invoked with the new token and
// base URL after a successful /wechat-login flow. baseURL is the iLink API base
// URL to use during the login flow. Typically wired to (*wechat.Bot).Reload by
// the server.
func (b *Bot) SetWeChatReloader(fn func(token, baseURL string), baseURL string) {
	b.wechatReloader = fn
	b.wechatBaseURL = baseURL
}

// New creates a new stream bot. Returns nil if ServerURL or BotToken is empty.
func New(cfg config.StreamConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer, commander Commander) *Bot {
	if cfg.ServerURL == "" || cfg.BotToken == "" {
		return nil
	}

	return &Bot{
		client:      NewClient(cfg.ServerURL, cfg.BotToken),
		starter:     starter,
		commander:   commander,
		convManager: convMgr,
		analyzer:    analyzer,
		convIDs:     cfg.ConversationIDs,
		botUserID:   extractBotUserID(cfg.BotToken),
	}
}

// Start begins listening on all configured conversations. Non-blocking.
func (b *Bot) Start(ctx context.Context) error {
	if len(b.convIDs) == 0 {
		slog.Info("stream bot: no conversation IDs configured, not starting")
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

	slog.Info("stream bot started", "conversations", len(b.convIDs))
	return nil
}

// SendNotification sends a message to all configured conversations as the bot.
// Intended for external systems (monitoring, cron jobs, etc.) to post messages.
func (b *Bot) SendNotification(ctx context.Context, message string) error {
	var lastErr error
	for _, convID := range b.convIDs {
		if err := b.client.SendMessage(ctx, convID, message, nil); err != nil {
			slog.Error("stream bot: failed to notify", "conversation_id", convID, "error", err)
			lastErr = err
		}
	}
	return lastErr
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	slog.Info("stream bot stopped")
}

// listenConversation connects to SSE for a single conversation and processes events.
func (b *Bot) listenConversation(ctx context.Context, convID string) {
	// Catch up: connect from seq 0, drain all existing events to find
	// the latest seq without processing them, so we only handle new messages.
	afterSeq := b.catchUpSeq(ctx, convID)
	slog.Info("stream bot caught up", "conversation_id", convID, "after_seq", afterSeq)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		events, err := b.client.StreamEvents(ctx, convID, afterSeq)
		if err != nil {
			slog.Error("stream bot SSE connect error", "conversation_id", convID, "error", err)
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
		slog.Info("stream bot SSE connection closed, reconnecting", "conversation_id", convID)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// catchUpSeq connects to SSE from seq 0, drains all existing events, and
// returns the latest seq so the bot only processes new messages.
func (b *Bot) catchUpSeq(ctx context.Context, convID string) int64 {
	catchUpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := b.client.StreamEvents(catchUpCtx, convID, 0)
	if err != nil {
		slog.Warn("stream bot catch-up failed", "conversation_id", convID, "error", err)
		return 0
	}

	var maxSeq int64
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return maxSeq
			}
			maxSeq = event.Seq
		case <-time.After(2 * time.Second):
			// No events for 2s — we've caught up
			return maxSeq
		}
	}
}

// messagePayload is the shape of a message.created event payload.
type messagePayload struct {
	UserID  string   `json:"user_id"`
	Content string   `json:"content"`
	FileIDs []string `json:"file_ids,omitempty"`
}

func (b *Bot) handleMessageEvent(ctx context.Context, convID string, event Event) {
	var msg messagePayload
	if err := json.Unmarshal(event.Payload, &msg); err != nil {
		slog.Error("stream bot: failed to parse message payload", "error", err)
		return
	}

	// Ignore own messages
	if msg.UserID == b.botUserID {
		return
	}

	text := strings.TrimSpace(msg.Content)

	// Download and inline any attached files
	if len(msg.FileIDs) > 0 {
		fileContent := b.resolveFiles(ctx, msg.FileIDs)
		if fileContent != "" {
			if text != "" {
				text = text + "\n\n" + fileContent
			} else {
				text = fileContent
			}
		}
	}

	if text == "" {
		return
	}

	b.handleMessage(ctx, convID, text)
}

// resolveFiles downloads files and returns their content formatted for the message.
// Text files are inlined; binary files are saved to a temp directory and referenced by path.
func (b *Bot) resolveFiles(ctx context.Context, fileIDs []string) string {
	var parts []string

	for _, fileID := range fileIDs {
		file, err := b.client.DownloadFile(ctx, fileID)
		if err != nil {
			slog.Error("stream bot: failed to download file", "file_id", fileID, "error", err)
			continue
		}

		if isTextContent(file.ContentType) {
			parts = append(parts, fmt.Sprintf("--- File: %s ---\n%s\n--- End: %s ---", file.Filename, string(file.Data), file.Filename))
		} else {
			// Save binary file to temp dir so the agent can access it
			path, err := saveToTemp(file)
			if err != nil {
				slog.Error("stream bot: failed to save file", "file", file.Filename, "error", err)
				parts = append(parts, fmt.Sprintf("[Attached file: %s (%s, %d bytes) — failed to save]", file.Filename, file.ContentType, len(file.Data)))
				continue
			}
			slog.Info("stream bot: saved file", "file", file.Filename, "path", path)
			if isImageContent(file.ContentType) {
				parts = append(parts, fmt.Sprintf("[Image: %s]", path))
			} else {
				parts = append(parts, fmt.Sprintf("[File '%s': %s]", file.Filename, path))
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

// saveToTemp writes a downloaded file to a temp directory and returns the path.
func saveToTemp(file *DownloadedFile) (string, error) {
	dir := filepath.Join(os.TempDir(), "agent-runner-files")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	// Use a unique prefix to avoid collisions
	safeName := filepath.Base(file.Filename)
	path := filepath.Join(dir, fmt.Sprintf("%d-%s", time.Now().UnixMilli(), safeName))

	if err := os.WriteFile(path, file.Data, 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return path, nil
}

// isTextContent returns true if the content type is text-based.
func isTextContent(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	textTypes := []string{
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-yaml",
		"application/yaml",
		"application/toml",
		"application/x-sh",
		"application/sql",
	}
	for _, t := range textTypes {
		if strings.HasPrefix(contentType, t) {
			return true
		}
	}
	return false
}

// isImageContent returns true if the content type is an image.
func isImageContent(contentType string) bool {
	return strings.HasPrefix(contentType, "image/")
}

func (b *Bot) handleMessage(ctx context.Context, convID, text string) {
	// Handle configuration commands before any LLM or conversation logic.
	if b.commander != nil {
		if reply, ok := b.commander.Handle(text); ok {
			b.emitFinal(ctx, convID, reply)
			return
		}
	}

	// Handle /cancel command
	if text == "/cancel" {
		b.convManager.Complete(convID)
		b.emitFinal(ctx, convID, "Conversation cancelled. Send a new message to start over.")
		return
	}

	// Handle /wechat-login: run the iLink QR login flow and hot-reload the WeChat bot.
	if text == "/wechat-login" {
		b.handleWeChatLogin(ctx, convID)
		return
	}

	conv := b.convManager.GetOrCreate(convID)
	conv.AddMessage("user", text)

	state := conv.GetState()

	if state == conversation.StateExecuting {
		b.emitFinal(ctx, convID, "Message queued — I'll process it after the current task finishes.")
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

	// If no analyzer is configured, skip analysis and execute directly
	if b.analyzer == nil {
		b.handleConfirmation(ctx, convID, conv)
		return
	}

	b.handleAnalysis(ctx, convID, conv)
}

func (b *Bot) handleConfirmation(ctx context.Context, convID string, conv *conversation.Conversation) {
	b.emitThinking(ctx, convID, "Working on it...")
	conv.SetState(conversation.StateExecuting)

	// Build message: latest user message + conversation history for context
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

	sessionID, err := b.starter.StartAgent(message, "stream")
	if err != nil {
		conv.SetState(conversation.StateGathering)
		b.emitFinal(ctx, convID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	slog.Info("stream bot: agent session started", "session_id", sessionID)

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(convID, sessionID)

		// Upload output files and send as message with attachments
		if session, ok := b.starter.GetAgentSession(sessionID); ok {
			// Add agent output to conversation history for future context
			for _, iter := range session.Iterations {
				if iter.Output != "" {
					conv.AddMessage("assistant", iter.Output)
				}
			}

			// Upload _send/ files
			if len(session.OutputFiles) > 0 {
				b.uploadOutputFiles(context.Background(), convID, session)
			}
		}

		// Summarize conversation if it's getting long
		if b.analyzer != nil && conv.NeedsCompaction() {
			b.summarizeConversation(conv)
		}

		// If user sent messages during execution, process them now
		if conv.ClearPendingInput() {
			conv.SetState(conversation.StateGathering)
			if b.analyzer == nil {
				b.handleConfirmation(context.Background(), convID, conv)
			} else {
				b.handleAnalysis(context.Background(), convID, conv)
			}
			return
		}

		b.convManager.Complete(convID)
	}()
}

func (b *Bot) handleAnalysis(ctx context.Context, convID string, conv *conversation.Conversation) {
	analysisCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := b.analyzer.Analyze(analysisCtx, conv)
	if err != nil {
		slog.Error("stream bot: analyzer error", "error", err)
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
		slog.Warn("stream bot: conversation summarization failed", "error", err)
		return
	}
	conv.CompactWithSummary(summary, keepRecent)
	slog.Info("stream bot: conversation compacted", "summary_len", len(summary), "kept_recent", keepRecent)
}

// uploadOutputFiles uploads output files from the agent session and sends them
// as a message with file attachments.
func (b *Bot) uploadOutputFiles(ctx context.Context, convID string, session *agent.Session) {
	var fileIDs []string
	var fileNames []string

	for _, f := range session.OutputFiles {
		fileID, err := b.client.UploadFile(ctx, convID, f.Name, f.ContentType, f.Data)
		if err != nil {
			slog.Error("stream bot: failed to upload file", "file", f.Name, "error", err)
			continue
		}
		slog.Info("stream bot: uploaded file", "file", f.Name, "file_id", fileID)
		fileIDs = append(fileIDs, fileID)
		fileNames = append(fileNames, f.Name)
	}

	if len(fileIDs) > 0 {
		msg := fmt.Sprintf("Generated %d file(s): %s", len(fileIDs), strings.Join(fileNames, ", "))
		if err := b.client.SendMessage(ctx, convID, msg, fileIDs); err != nil {
			slog.Error("stream bot: failed to send message with files", "error", err)
		}
	}
}

func (b *Bot) pollAndReport(convID, sessionID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	ctx := context.Background()

	for range ticker.C {
		session, exists := b.starter.GetAgentSession(sessionID)
		if !exists {
			b.emitFinal(ctx, convID, "Session not found.")
			return
		}

		if session.Status == agent.SessionStatusCompleted || session.Status == agent.SessionStatusFailed {
			b.emitFinal(ctx, convID, formatFinalResult(session))
			return
		}
	}
}

// Event emission helpers

func (b *Bot) emitThinking(ctx context.Context, convID, msg string) {
	b.emit(ctx, convID, "status.thinking", map[string]string{"message": msg})
}

func (b *Bot) emitDelta(ctx context.Context, convID, text string) {
	b.emit(ctx, convID, "assistant.delta", map[string]string{"delta": text})
}

func (b *Bot) emitFinal(ctx context.Context, convID, text string) {
	b.emit(ctx, convID, "assistant.final", map[string]string{"content": text})
}

func (b *Bot) emit(ctx context.Context, convID, eventType string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("stream bot: marshal error", "error", err)
		return
	}
	if err := b.client.EmitEvent(ctx, convID, eventType, data); err != nil {
		slog.Error("stream bot: emit error", "event_type", eventType, "error", err)
	}
}

// Formatting helpers

func formatFinalResult(session *agent.Session) string {
	var sb strings.Builder

	// Include the last iteration's output so the user sees Claude's response
	if len(session.Iterations) > 0 {
		lastOutput := session.Iterations[len(session.Iterations)-1].Output
		if lastOutput != "" {
			if len(lastOutput) > 4000 {
				lastOutput = lastOutput[:4000] + "\n... (truncated)"
			}
			sb.WriteString(lastOutput)
			sb.WriteString("\n\n---\n")
		}
	}

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

// handleWeChatLogin runs the iLink QR login flow in a background goroutine and
// hot-reloads the WeChat bot on success. The QR code is sent as a tappable text
// link (no CDN upload required from stream).
func (b *Bot) handleWeChatLogin(ctx context.Context, convID string) {
	if b.wechatReloader == nil {
		b.emitFinal(ctx, convID, "WeChat bot is not configured on this server.")
		return
	}

	if !b.wechatLoginMu.TryLock() {
		b.emitFinal(ctx, convID, "A WeChat login is already in progress. Please wait.")
		return
	}

	b.emitFinal(ctx, convID, "Starting WeChat login flow...")

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.wechatLoginMu.Unlock()

		send := func(msg string) {
			b.emitFinal(ctx, convID, msg)
		}
		sendQR := func(qrCtx context.Context, qrContent string) {
			pngBytes, err := qrcode.Encode(qrContent, qrcode.Medium, 256)
			if err != nil {
				slog.Error("stream: failed to generate qr code image", "error", err)
				send("Tap the link below in WeChat to authorize the bot login:\n\n" + qrContent)
				return
			}
			fileID, err := b.client.UploadFile(qrCtx, convID, "qrcode.png", "image/png", pngBytes)
			if err != nil {
				slog.Error("stream: failed to upload qr code image", "error", err)
				send("Tap the link below in WeChat to authorize the bot login:\n\n" + qrContent)
				return
			}
			if err := b.client.SendMessage(qrCtx, convID, "Scan the QR code in WeChat to log in:", []string{fileID}); err != nil {
				slog.Error("stream: failed to send qr code message", "error", err)
			}
		}

		result, err := wechat.RunLoginFlow(ctx, b.wechatBaseURL, send, sendQR)
		if err != nil {
			slog.Error("stream: wechat login flow failed", "error", err)
			b.emitFinal(ctx, convID, "Login failed: "+err.Error())
			return
		}

		if err := config.SetEnvLocal("WECHAT_TOKEN", result.Token); err != nil {
			slog.Error("stream: failed to save wechat token to .env.local", "error", err)
			b.emitFinal(ctx, convID, "Login succeeded but could not save token: "+err.Error())
			return
		}
		if result.BaseURL != "" {
			if err := config.SetEnvLocal("WECHAT_BASE_URL", result.BaseURL); err != nil {
				slog.Warn("stream: failed to save wechat base_url to .env.local", "error", err)
			}
		}

		b.wechatReloader(result.Token, result.BaseURL)
		b.emitFinal(ctx, convID, "WeChat login successful! Bot is now active.")
	}()
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
