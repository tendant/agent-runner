package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/botcommon"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter = botcommon.AgentStarter

// Gateway routes incoming messages through command dispatch before any
// conversation or agent logic. It is the single entry point for all messages.
type Gateway = botcommon.Gateway

// Bot is a Telegram bot that bridges messages to the agent runner.
type Bot struct {
	token     string
	chatID    int64
	mediaDir  string // directory for downloaded media files
	starter   AgentStarter
	gateway Gateway

	convManager *conversation.Manager
	analyzer    *conversation.Analyzer

	api    *tgbotapi.BotAPI
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Telegram bot. Returns nil if the token is empty.
// tmpRoot is the base directory for downloaded media files.
// The actual API connection is deferred to Start().
func New(cfg config.TelegramConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer, tmpRoot string, gateway Gateway) *Bot {
	if cfg.BotToken == "" {
		return nil
	}

	return &Bot{
		token:       cfg.BotToken,
		chatID:      cfg.ChatID,
		mediaDir:    filepath.Join(tmpRoot, "telegram-media"),
		starter:     starter,
		gateway:     gateway,
		convManager: convMgr,
		analyzer:    analyzer,
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

	slog.Info("telegram bot started", "username", b.api.Self.UserName)
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
	slog.Info("telegram bot stopped")
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// Security: only respond to the configured chat ID
	if b.chatID != 0 && msg.Chat.ID != b.chatID {
		slog.Warn("telegram: ignoring message from unauthorized chat", "chat_id", msg.Chat.ID)
		return
	}

	tgChatID := msg.Chat.ID
	chatID := strconv.FormatInt(tgChatID, 10)

	// Handle /start command
	if msg.IsCommand() && msg.Command() == "start" {
		b.send(tgChatID, "Agent Runner bot ready. Send a message to start a conversation.")
		return
	}

	content := b.extractContent(msg)
	if content == "" {
		return
	}

	// Route through the unified gateway: /cancel, commands, slash-block.
	if b.gateway != nil {
		asyncSend := func(msg string) { b.send(tgChatID, msg) }
		reset := func() { b.convManager.Complete(chatID) }
		if reply, _, ok := b.gateway.Handle(content, asyncSend, reset); ok {
			b.send(tgChatID, reply)
			return
		}
	}

	// Get or create conversation
	conv := b.convManager.GetOrCreate(chatID)
	conv.AddMessage("user", content)

	state := conv.GetState()

	// If currently executing, queue the message
	if state == conversation.StateExecuting {
		b.send(tgChatID, "Message queued — I'll process it after the current task finishes.")
		return
	}

	// If confirming, check for yes/no
	if state == conversation.StateConfirming {
		if isConfirmation(content) {
			b.handleConfirmation(tgChatID, chatID, conv)
			return
		}
		if isDenial(content) {
			conv.SetState(conversation.StateGathering)
			conv.AddMessage("assistant", "OK, what would you like to change?")
			b.send(tgChatID, "OK, what would you like to change?")
			return
		}
		// Not a clear yes/no — treat as continued conversation
	}

	// If no analyzer is configured, execute directly.
	if b.analyzer == nil {
		b.handleConfirmation(tgChatID, chatID, conv)
		return
	}

	// Analyze conversation via Claude (slow — acknowledge first)
	b.send(tgChatID, "Thinking...")
	b.handleAnalysis(tgChatID, chatID, conv)
}

// handleConfirmation starts the agent after the user confirms the plan.
func (b *Bot) handleConfirmation(tgChatID int64, chatID string, conv *conversation.Conversation) {
	b.send(tgChatID, "Starting agent...")
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

	sessionID, err := b.starter.StartAgent(message, "telegram")
	if err != nil {
		conv.SetState(conversation.StateGathering)
		b.send(tgChatID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	b.send(tgChatID, fmt.Sprintf("Agent session started: `%s`", sessionID))

	// Poll and report in background
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.pollAndReport(tgChatID, sessionID)

		session, sessionOk := b.starter.GetAgentSession(sessionID)
		if sessionOk {
			for _, iter := range session.Iterations {
				if iter.Output != "" {
					conv.AddMessage("assistant", iter.Output)
				}
			}
		}
		hasPending := conv.ClearPendingInput()

		// Clear StateExecuting before slow post-processing so new messages
		// are accepted immediately instead of being queued.
		if !hasPending {
			b.convManager.Complete(chatID)
		}

		if sessionOk && len(session.OutputFiles) > 0 {
			b.uploadOutputFiles(tgChatID, session)
		}
		if b.analyzer != nil && conv.NeedsCompaction() {
			b.summarizeConversation(conv)
		}

		if hasPending {
			conv.SetState(conversation.StateGathering)
			b.send(tgChatID, "Processing queued messages...")
			b.handleAnalysis(tgChatID, chatID, conv)
		}
	}()
}

// handleAnalysis calls the analyzer to decide the next action.
func (b *Bot) handleAnalysis(tgChatID int64, chatID string, conv *conversation.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := b.analyzer.Analyze(ctx, conv)
	if err != nil {
		slog.Error("telegram: analyzer error", "error", err)
		if ctx.Err() == context.DeadlineExceeded {
			b.send(tgChatID, "Sorry, the request timed out. Please try again.")
		} else {
			b.send(tgChatID, "Sorry, I had trouble understanding that. Could you rephrase?")
		}
		return
	}

	switch result.Action {
	case "execute":
		// High-confidence action — skip confirmation, run immediately
		conv.AddMessage("assistant", result.Message)
		b.send(tgChatID, result.Message)
		b.handleConfirmation(tgChatID, chatID, conv)

	case "ask":
		conv.AddMessage("assistant", result.Message)
		b.send(tgChatID, result.Message)

	case "plan":
		conv.SetPlan(result.Message)
		conv.AddMessage("assistant", result.Message)
		b.send(tgChatID, result.Message+"\n\nProceed? (yes/no)")

	default:
		// Unknown action — treat as "ask"
		conv.AddMessage("assistant", result.Message)
		b.send(tgChatID, result.Message)
	}
}

// telegramReporter adapts botcommon.PollAndReport's callbacks to Telegram
// message sends. Telegram reports progress per completed iteration, so
// OnIterationStart is unused.
type telegramReporter struct {
	bot    *Bot
	chatID int64
}

func (r *telegramReporter) OnIterationComplete(iter agent.IterationResult) {
	r.bot.send(r.chatID, FormatIteration(iter))
}
func (r *telegramReporter) OnIterationStart(current, max int) {}
func (r *telegramReporter) OnFinal(session *agent.Session) {
	r.bot.send(r.chatID, FormatFinalResult(session))
}
func (r *telegramReporter) OnNotFound() { r.bot.send(r.chatID, "Session not found.") }
func (r *telegramReporter) OnTimeout() {
	r.bot.send(r.chatID, "Session timed out waiting for a response.")
}

func (b *Bot) pollAndReport(tgChatID int64, sessionID string) {
	botcommon.PollAndReport(b.starter, sessionID, &telegramReporter{bot: b, chatID: tgChatID})
}

// extractContent assembles a text representation of a Telegram message.
// Text and caption are included as-is. Photos, voice, audio, documents, and
// video are downloaded to b.mediaDir and represented as path annotations so
// the agent can reference them (same format WeChat uses: [Image: /path], etc.).
func (b *Bot) extractContent(msg *tgbotapi.Message) string {
	var parts []string

	// Plain text or caption attached to a media message.
	// For bot commands, normalize "/cmd@BotName args" → "/cmd args" so the
	// gateway receives clean text regardless of whether we're in a group chat.
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	if msg.IsCommand() {
		text = "/" + msg.Command()
		if args := msg.CommandArguments(); args != "" {
			text += " " + args
		}
	}
	if text != "" {
		parts = append(parts, text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Photos — use the highest-resolution variant.
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		path, err := b.downloadFile(ctx, photo.FileID, "photo-"+photo.FileUniqueID+".jpg")
		if err != nil {
			slog.Warn("telegram: photo download failed", "error", err)
			parts = append(parts, "[Image — could not download]")
		} else {
			parts = append(parts, fmt.Sprintf("[Image: %s]", path))
		}
	}

	// Voice messages (OGG/Opus — can be transcribed with Whisper if needed).
	if msg.Voice != nil {
		path, err := b.downloadFile(ctx, msg.Voice.FileID, "voice-"+msg.Voice.FileUniqueID+".ogg")
		if err != nil {
			slog.Warn("telegram: voice download failed", "error", err)
			parts = append(parts, fmt.Sprintf("[Voice, %ds — could not download]", msg.Voice.Duration))
		} else {
			parts = append(parts, fmt.Sprintf("[Voice, %ds (OGG/Opus): %s]", msg.Voice.Duration, path))
		}
	}

	// Audio files.
	if msg.Audio != nil {
		name := msg.Audio.FileName
		if name == "" {
			name = "audio-" + msg.Audio.FileUniqueID
		}
		path, err := b.downloadFile(ctx, msg.Audio.FileID, name)
		if err != nil {
			slog.Warn("telegram: audio download failed", "error", err)
			parts = append(parts, fmt.Sprintf("[Audio '%s', %ds — could not download]", msg.Audio.Title, msg.Audio.Duration))
		} else {
			parts = append(parts, fmt.Sprintf("[Audio '%s', %ds: %s]", msg.Audio.Title, msg.Audio.Duration, path))
		}
	}

	// Documents / arbitrary files.
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "file-" + msg.Document.FileUniqueID
		}
		path, err := b.downloadFile(ctx, msg.Document.FileID, name)
		if err != nil {
			slog.Warn("telegram: document download failed", "file", name, "error", err)
			parts = append(parts, fmt.Sprintf("[File '%s' — could not download]", name))
		} else {
			parts = append(parts, fmt.Sprintf("[File '%s': %s]", name, path))
		}
	}

	// Video.
	if msg.Video != nil {
		name := "video-" + msg.Video.FileUniqueID + ".mp4"
		path, err := b.downloadFile(ctx, msg.Video.FileID, name)
		if err != nil {
			slog.Warn("telegram: video download failed", "error", err)
			parts = append(parts, fmt.Sprintf("[Video, %ds — could not download]", msg.Video.Duration))
		} else {
			parts = append(parts, fmt.Sprintf("[Video, %ds: %s]", msg.Video.Duration, path))
		}
	}

	return strings.Join(parts, "\n")
}

// downloadFile downloads a Telegram file by FileID and saves it to b.mediaDir.
// Returns the absolute path to the saved file.
func (b *Bot) downloadFile(ctx context.Context, fileID, filename string) (string, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get file info: %w", err)
	}
	downloadURL, err := b.api.GetFileDirectURL(file.FilePath)
	if err != nil {
		return "", fmt.Errorf("get download url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	if err := os.MkdirAll(b.mediaDir, 0755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	savePath := filepath.Join(b.mediaDir, filepath.Base(filename))
	if err := os.WriteFile(savePath, data, 0644); err != nil {
		return "", fmt.Errorf("save file: %w", err)
	}

	absPath, err := filepath.Abs(savePath)
	if err != nil {
		return savePath, nil
	}
	return absPath, nil
}

// SendNotification implements the api.Notifier interface by sending to the
// configured default chat ID. Returns an error if no chat ID is set or the bot
// is not connected.
func (b *Bot) SendNotification(_ context.Context, message string) error {
	if b.chatID == 0 {
		return fmt.Errorf("telegram: no default chat ID configured")
	}
	if b.api == nil {
		return fmt.Errorf("telegram: bot not connected")
	}
	b.send(b.chatID, message)
	return nil
}

func (b *Bot) send(chatID int64, text string) {
	if b.api == nil {
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	if _, err := b.api.Send(msg); err != nil {
		slog.Error("telegram: failed to send message", "error", err)
	}
}

// uploadOutputFiles sends _send/ files as Telegram documents.
func (b *Bot) uploadOutputFiles(chatID int64, session *agent.Session) {
	for _, f := range session.OutputFiles {
		fileBytes := tgbotapi.FileBytes{Name: f.Name, Bytes: f.Data}
		doc := tgbotapi.NewDocument(chatID, fileBytes)
		doc.Caption = f.Name
		if _, err := b.api.Send(doc); err != nil {
			slog.Error("telegram: failed to send file", "file", f.Name, "error", err)
		} else {
			slog.Info("telegram: sent file", "file", f.Name)
		}
	}
}


// summarizeConversation compacts old messages into a summary.
func (b *Bot) summarizeConversation(conv *conversation.Conversation) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := conv.GetMessages()
	// Summarize the older half, keep the recent half
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
		slog.Warn("telegram: conversation summarization failed", "error", err)
		return
	}
	conv.CompactWithSummary(summary, keepRecent)
	slog.Info("telegram: conversation compacted", "summary_len", len(summary), "kept_recent", keepRecent)
}

// FormatIteration formats a single iteration result for Telegram. Commit
// hashes are wrapped in Markdown backticks since telegram sends with
// ParseMode = tgbotapi.ModeMarkdown.
func FormatIteration(iter agent.IterationResult) string {
	return botcommon.FormatIterationCore(iter, true)
}

// FormatFinalResult formats the final session summary for Telegram.
func FormatFinalResult(session *agent.Session) string {
	sb := botcommon.FormatStatusLine(session)
	if len(session.OutputFiles) > 0 {
		sb += fmt.Sprintf("\n\n%d file(s) attached", len(session.OutputFiles))
	}
	sb += botcommon.FormatWarningsSuffix(session)
	return sb
}

// isConfirmation checks if the message is a positive confirmation.
func isConfirmation(text string) bool {
	return botcommon.IsConfirmation(text)
}

// isDenial checks if the message is a negative response.
func isDenial(text string) bool {
	return botcommon.IsDenial(text)
}
