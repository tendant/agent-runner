package wechat

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

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
	client     *Client
	downloader *Downloader
	starter    AgentStarter

	convManager *conversation.Manager
	analyzer    *conversation.Analyzer

	// contextTokens maps fromUserID → most-recently-received context_token.
	// The token must be echoed in replies so the iLink server can route them.
	tokenMu   sync.Mutex
	ctxTokens map[string]string

	// loginMu prevents concurrent /wechat-login flows.
	loginMu sync.Mutex

	// reloadMu guards cancel to prevent a race between Reload and Start.
	reloadMu sync.Mutex

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new WeChat bot. Always returns a non-nil bot; if no token is
// configured the bot starts in a dormant state and becomes active after
// Reload is called with a valid token.
func New(cfg config.WeChatConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer) *Bot {
	mediaDir := filepath.Join(cfg.StateDir, "wechat-media")
	return &Bot{
		client:      NewClient(cfg.BaseURL, cfg.Token, cfg.StateDir),
		downloader:  NewDownloader(cfg.CDNBaseURL, mediaDir),
		starter:     starter,
		convManager: convMgr,
		analyzer:    analyzer,
		ctxTokens:   make(map[string]string),
	}
}

// Reload updates the bot's credentials at runtime and restarts the poll loop
// with the new token. Always cancels any existing loop so stale long-polls
// using the old token are dropped immediately.
func (b *Bot) Reload(token, baseURL string) {
	b.client.SetToken(token, baseURL)
	slog.Info("wechat: credentials reloaded", "has_base_url", baseURL != "")

	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()

	// Cancel the existing poll loop if running so it exits cleanly.
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runLoop(ctx)
	}()
	slog.Info("wechat: poll loop restarted with new token")
}

// Start begins long-polling the iLink API. Non-blocking. If no token is
// configured the bot waits dormant until Reload is called.
func (b *Bot) Start(ctx context.Context) error {
	b.client.mu.RLock()
	token := b.client.token
	b.client.mu.RUnlock()

	if token == "" {
		slog.Info("wechat bot: no token configured, waiting for /wechat-login")
		return nil
	}

	b.reloadMu.Lock()
	ctx, b.cancel = context.WithCancel(ctx)
	b.reloadMu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.runLoop(ctx)
	}()

	slog.Info("wechat bot started", "base_url", b.client.baseURL)
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
	buf := b.client.loadSyncBuf()  // restore cursor from disk across restarts
	pollCount := 0
	consecutiveFailures := 0

	slog.Info("wechat: poll loop starting", "has_saved_cursor", buf != "")

	for {
		if ctx.Err() != nil {
			slog.Info("wechat: poll loop stopping", "polls", pollCount)
			return
		}

		pollCount++
		resp, err := b.client.GetUpdates(ctx, buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			delay := 3 * time.Second
			if consecutiveFailures >= 3 {
				delay = 30 * time.Second
				slog.Warn("wechat: repeated getupdates errors, backing off 30s",
					"error", err, "polls", pollCount, "failures", consecutiveFailures)
			} else {
				slog.Warn("wechat: getupdates error, retrying",
					"error", err, "polls", pollCount, "delay", delay)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		consecutiveFailures = 0

		// Handle API-level errors returned inside the response body.
		if resp.Ret != 0 || resp.ErrCode != 0 {
			if resp.ErrCode == ErrCodeSessionExpired || resp.Ret == ErrCodeSessionExpired {
				slog.Error("wechat: session expired (errcode -14) — re-run wechat-login to get a new token",
					"polls", pollCount)
				return // stop polling; token is no longer valid
			}
			slog.Error("wechat: getupdates api error",
				"ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg, "polls", pollCount)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Respect server-suggested poll timeout for the next request.
		if resp.LongpollingTimeoutMs > 0 {
			b.client.setPollTimeout(time.Duration(resp.LongpollingTimeoutMs) * time.Millisecond)
		}

		if resp.GetUpdatesBuf != "" {
			slog.Debug("wechat: cursor updated", "buf_len", len(resp.GetUpdatesBuf))
			buf = resp.GetUpdatesBuf
			b.client.saveSyncBuf(buf)
		} else {
			slog.Debug("wechat: empty poll (no new messages)", "polls", pollCount)
		}

		if len(resp.Msgs) > 0 {
			slog.Info("wechat: received messages", "count", len(resp.Msgs))
		}

		for _, msg := range resp.Msgs {
			slog.Info("wechat: inbound message",
				"from_user_id", msg.FromUserID,
				"message_type", msg.MessageType,
				"items", len(msg.ItemList),
				"has_context_token", msg.ContextToken != "",
			)
			b.storeContextToken(msg.FromUserID, msg.ContextToken)
			b.handleMessage(msg)
		}
	}
}

// storeContextToken persists the latest context_token for a user.
func (b *Bot) storeContextToken(userID, token string) {
	if userID == "" || token == "" {
		slog.Warn("wechat: missing user_id or context_token on inbound message",
			"has_user_id", userID != "", "has_token", token != "")
		return
	}
	b.tokenMu.Lock()
	b.ctxTokens[userID] = token
	b.tokenMu.Unlock()
	slog.Debug("wechat: context_token stored", "user_id", userID)
}

func (b *Bot) getContextToken(userID string) string {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	return b.ctxTokens[userID]
}

// handleMessage processes a single inbound WeChat message.
func (b *Bot) handleMessage(msg WeixinMessage) {
	if msg.MessageType != MessageTypeUser {
		slog.Info("wechat: ignoring non-user message", "from_user_id", msg.FromUserID, "message_type", msg.MessageType)
		return // only handle inbound user messages
	}

	ctx := context.Background()
	content := b.extractContent(ctx, msg)
	if content == "" {
		slog.Warn("wechat: received message with no usable content", "from_user_id", msg.FromUserID)
		return
	}

	slog.Info("wechat: handling message", "from_user_id", msg.FromUserID, "content_len", len(content))

	userID := msg.FromUserID
	chatID := userID // use WeChat user ID as conversation key

	// Handle /cancel command
	if strings.EqualFold(strings.TrimSpace(content), "/cancel") {
		b.convManager.Complete(chatID)
		b.sendText(ctx, userID, "Conversation cancelled. Send a new message to start over.")
		return
	}

	// Handle /wechat-login command — runs the QR login flow inline.
	if strings.EqualFold(strings.TrimSpace(content), "/wechat-login") {
		b.handleLogin(userID)
		return
	}

	conv := b.convManager.GetOrCreate(chatID)
	conv.AddMessage("user", content)

	state := conv.GetState()
	slog.Info("wechat: conversation state", "user_id", userID, "state", state)

	if state == conversation.StateExecuting {
		b.sendText(ctx, userID, "Message queued — I'll process it after the current task finishes.")
		return
	}

	if state == conversation.StateConfirming {
		if isConfirmation(content) {
			b.handleConfirmation(userID, chatID, conv)
			return
		}
		if isDenial(content) {
			conv.SetState(conversation.StateGathering)
			conv.AddMessage("assistant", "OK, what would you like to change?")
			b.sendText(ctx, userID, "OK, what would you like to change?")
			return
		}
	}

	b.sendText(ctx, userID, "Thinking...")
	b.handleAnalysis(userID, chatID, conv)
}

// handleLogin runs the iLink QR login flow in a background goroutine and sends
// status updates back to the user. On success the new token (and optional
// region-specific base URL) are written to .env.local.
func (b *Bot) handleLogin(userID string) {
	if !b.loginMu.TryLock() {
		b.sendText(context.Background(), userID, "A login is already in progress. Please wait.")
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.loginMu.Unlock()

		send := func(msg string) {
			b.sendText(context.Background(), userID, msg)
		}

		send("Starting WeChat login flow...")

		// sendQR attempts to generate a QR code PNG, upload it to the CDN, and
		// send it as an image message. Falls back to a tappable text link on error.
		sendQR := func(ctx context.Context, qrContent string) {
			pngBytes, err := qrcode.Encode(qrContent, qrcode.Medium, 256)
			if err != nil {
				slog.Warn("wechat: qr encode failed, using text fallback", "error", err)
				sendQRText(send, qrContent)
				return
			}
			uploaded, err := UploadImage(ctx, b.client, b.downloader.cdnBaseURL, userID, pngBytes)
			if err != nil {
				slog.Warn("wechat: qr image upload failed, using text fallback", "error", err)
				sendQRText(send, qrContent)
				return
			}
			ctxToken := b.getContextToken(userID)
			if err := b.client.SendImage(ctx, userID, uploaded.DownloadParam, uploaded.AESKeyHex, ctxToken, uploaded.CiphertextSize); err != nil {
				slog.Warn("wechat: qr send image failed, using text fallback", "error", err)
				sendQRText(send, qrContent)
				return
			}
			// Also send the text link as backup so the user can tap it if the image is unclear.
			send("Or tap this link to authorize:\n\n" + qrContent)
		}

		result, err := RunLoginFlow(context.Background(), b.client.baseURL, send, sendQR)
		if err != nil {
			slog.Error("wechat: login flow failed", "error", err)
			send("Login failed: " + err.Error())
			return
		}

		// Persist the new token to .env.local so it survives restarts.
		if err := config.SetEnvLocal("WECHAT_TOKEN", result.Token); err != nil {
			slog.Error("wechat: failed to save token to .env.local", "error", err)
			send("Login succeeded but could not save token: " + err.Error())
			return
		}
		if result.BaseURL != "" && result.BaseURL != b.client.baseURL {
			if err := config.SetEnvLocal("WECHAT_BASE_URL", result.BaseURL); err != nil {
				slog.Warn("wechat: failed to save base_url to .env.local", "error", err)
			}
		}

		slog.Info("wechat: login complete, token saved to .env.local")
		send("Login successful! Token saved to .env.local.\n\nRestart the server to connect with the new token.")
	}()
}

// handleConfirmation starts the agent after the user confirms.
func (b *Bot) handleConfirmation(userID, chatID string, conv *conversation.Conversation) {
	slog.Info("wechat: starting agent", "user_id", userID)
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
		slog.Error("wechat: failed to start agent", "user_id", userID, "error", err)
		conv.SetState(conversation.StateGathering)
		b.sendText(context.Background(), userID, fmt.Sprintf("Failed to start agent: %s", err))
		return
	}

	slog.Info("wechat: agent session started", "user_id", userID, "session_id", sessionID)
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
				b.sendOutputFiles(context.Background(), userID, session.OutputFiles)
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
	if token == "" {
		slog.Warn("wechat: sending message without context_token — reply routing may fail", "user_id", userID)
	}
	if err := b.client.SendMessage(ctx, userID, text, token); err != nil {
		slog.Error("wechat: failed to send message", "user_id", userID, "error", err)
	}
}

// sendOutputFiles delivers each agent output file to the WeChat user.
// Images are sent via SendImage; other files via SendFile.
func (b *Bot) sendOutputFiles(ctx context.Context, userID string, files []agent.OutputFile) {
	token := b.getContextToken(userID)
	cdnBaseURL := b.downloader.cdnBaseURL

	for _, f := range files {
		if isImageContentType(f.ContentType) {
			uploaded, err := UploadImage(ctx, b.client, cdnBaseURL, userID, f.Data)
			if err != nil {
				slog.Warn("wechat: output image upload failed, falling back to text", "file", f.Name, "error", err)
				b.sendText(ctx, userID, fmt.Sprintf("[Image: %s — upload failed]", f.Name))
				continue
			}
			if err := b.client.SendImage(ctx, userID, uploaded.DownloadParam, uploaded.AESKeyHex, token, uploaded.CiphertextSize); err != nil {
				slog.Error("wechat: failed to send output image", "file", f.Name, "error", err)
			}
		} else {
			uploaded, err := UploadFile(ctx, b.client, cdnBaseURL, userID, f.Data)
			if err != nil {
				slog.Warn("wechat: output file upload failed, falling back to text", "file", f.Name, "error", err)
				b.sendText(ctx, userID, fmt.Sprintf("[File: %s — upload failed]", f.Name))
				continue
			}
			if err := b.client.SendFile(ctx, userID, f.Name, uploaded.DownloadParam, uploaded.AESKeyHex, uploaded.RawMD5Hex, token, uploaded.CiphertextSize); err != nil {
				slog.Error("wechat: failed to send output file", "file", f.Name, "error", err)
			}
		}
	}
}

// isImageContentType returns true for common image MIME types.
func isImageContentType(ct string) bool {
	return ct == "image/jpeg" || ct == "image/png" || ct == "image/gif" ||
		ct == "image/webp" || ct == "image/bmp" || ct == "image/tiff"
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

// extractContent assembles the full content of an inbound message. Text items
// are included as-is; media items are downloaded and represented as file-path
// annotations. Voice items with a transcript use the transcript directly.
// Returns "" if the message has no usable content.
func (b *Bot) extractContent(ctx context.Context, msg WeixinMessage) string {
	var parts []string
	for _, item := range msg.ItemList {
		switch item.Type {
		case MessageItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				parts = append(parts, item.TextItem.Text)
			}

		case MessageItemTypeImage:
			path, err := b.downloader.DownloadImage(ctx, item)
			if err != nil {
				slog.Warn("wechat: image download failed", "error", err)
				parts = append(parts, "[Image — could not download]")
			} else {
				parts = append(parts, fmt.Sprintf("[Image: %s]", path))
			}

		case MessageItemTypeVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				// WeChat's iLink API performs server-side speech-to-text and
				// populates VoiceItem.Text. This is the normal path for WeChat
				// voice messages — no local audio processing needed.
				parts = append(parts, item.VoiceItem.Text)
			} else {
				// Fallback: no server-side transcript (unusual for WeChat; may
				// occur for other future clients). Download the raw SILK file
				// and pass the path so the agent can process it.
				// TODO: add bot-layer ffmpeg + Whisper transcription here when
				// a client that regularly omits transcripts is in scope.
				path, err := b.downloader.DownloadVoice(ctx, item)
				if err != nil {
					slog.Warn("wechat: voice download failed", "error", err)
					dur := ""
					if item.VoiceItem != nil && item.VoiceItem.Playtime > 0 {
						dur = fmt.Sprintf(" (%ds)", item.VoiceItem.Playtime/1000)
					}
					parts = append(parts, "[Voice message"+dur+" — could not download]")
				} else {
					dur := ""
					if item.VoiceItem != nil && item.VoiceItem.Playtime > 0 {
						dur = fmt.Sprintf(", %ds", item.VoiceItem.Playtime/1000)
					}
					parts = append(parts, fmt.Sprintf("[Voice%s (SILK format, requires ffmpeg to convert): %s]", dur, path))
				}
			}

		case MessageItemTypeFile:
			path, err := b.downloader.DownloadFile(ctx, item)
			if err != nil {
				slog.Warn("wechat: file download failed", "error", err)
				name := ""
				if item.FileItem != nil {
					name = item.FileItem.FileName
				}
				if name != "" {
					parts = append(parts, fmt.Sprintf("[File '%s' — could not download]", name))
				} else {
					parts = append(parts, "[File — could not download]")
				}
			} else {
				name := ""
				if item.FileItem != nil {
					name = item.FileItem.FileName
				}
				if name != "" {
					parts = append(parts, fmt.Sprintf("[File '%s': %s]", name, path))
				} else {
					parts = append(parts, fmt.Sprintf("[File: %s]", path))
				}
			}

		case MessageItemTypeVideo:
			path, err := b.downloader.DownloadVideo(ctx, item)
			if err != nil {
				slog.Warn("wechat: video download failed", "error", err)
				parts = append(parts, "[Video — could not download]")
			} else {
				parts = append(parts, fmt.Sprintf("[Video: %s]", path))
			}

		default:
			slog.Debug("wechat: unknown item type", "type", item.Type)
		}
	}
	return strings.Join(parts, "\n")
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
		fmt.Fprintf(&sb, "\n\n%d file(s) sent", len(session.OutputFiles))
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
