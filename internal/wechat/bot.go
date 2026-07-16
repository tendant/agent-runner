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
	"github.com/agent-runner/agent-runner/internal/botcommon"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter = botcommon.AgentStarter

// Gateway routes incoming messages through command dispatch before any
// conversation or agent logic. It is the single entry point for all messages.
type Gateway = botcommon.Gateway

// Bot is a WeChat bot that bridges messages to the agent runner via the
// Tencent iLink API.
type Bot struct {
	client     *Client
	downloader *Downloader
	starter    AgentStarter
	gateway   Gateway

	convManager *conversation.Manager
	analyzer    *conversation.Analyzer

	// contextTokens maps fromUserID → most-recently-received context_token.
	// The token must be echoed in replies so the iLink server can route them.
	// ctxTokenTimes records when each token was last received so stale tokens
	// can be detected and retried without the token on API failure.
	tokenMu       sync.Mutex
	ctxTokens     map[string]string
	ctxTokenTimes map[string]time.Time

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
func New(cfg config.WeChatConfig, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer, gateway Gateway) *Bot {
	mediaDir := filepath.Join(cfg.StateDir, "wechat-media")
	return &Bot{
		client:        NewClient(cfg.BaseURL, cfg.Token, cfg.StateDir),
		downloader:    NewDownloader(cfg.CDNBaseURL, mediaDir),
		starter:       starter,
		gateway:      gateway,
		convManager:   convMgr,
		analyzer:      analyzer,
		ctxTokens:     make(map[string]string),
		ctxTokenTimes: make(map[string]time.Time),
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
	b.ctxTokenTimes[userID] = time.Now()
	b.tokenMu.Unlock()
	slog.Debug("wechat: context_token stored", "user_id", userID)
}

func (b *Bot) getContextToken(userID string) string {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	return b.ctxTokens[userID]
}

// getContextTokenWithAge returns the stored token and how long ago it was received.
func (b *Bot) getContextTokenWithAge(userID string) (string, time.Duration) {
	b.tokenMu.Lock()
	defer b.tokenMu.Unlock()
	token := b.ctxTokens[userID]
	age := time.Since(b.ctxTokenTimes[userID])
	return token, age
}

// staleContextTokenAge is the threshold at which a context_token is considered
// potentially expired. WeChat tokens are session-scoped but may lapse during
// long-running agent sessions where the user sends no new messages.
const staleContextTokenAge = 15 * time.Minute

// sendWithRetry calls send(token) using the stored context_token for userID.
// If the send returns an API error (WeChat rejected the request), it retries
// once without the token so the message can still reach the user unthreaded.
// Network/timeout errors are not retried to avoid duplicate delivery.
// ctx is captured by the send closure; it is not used by sendWithRetry itself.
func (b *Bot) sendWithRetry(userID string, send func(token string) error) error {
	token, age := b.getContextTokenWithAge(userID)
	if token == "" {
		slog.Warn("wechat: sending without context_token — reply routing may fail", "user_id", userID)
	} else if age > staleContextTokenAge {
		slog.Warn("wechat: context_token is stale", "user_id", userID, "age", age.Round(time.Second))
	}
	err := send(token)
	if err != nil && token != "" && isAPIError(err) {
		slog.Warn("wechat: send failed with context_token, retrying without", "user_id", userID, "error", err)
		err = send("")
	}
	return err
}

// isAPIError reports whether err is a WeChat API-level rejection (errcode != 0),
// as opposed to a network or timeout error. Only API errors are safe to retry
// because a network error may mean the first request succeeded.
func isAPIError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "api error")
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

	// /wechat-login runs a channel-specific QR flow — handle before the gateway.
	if strings.EqualFold(strings.TrimSpace(content), "/wechat-login") {
		b.handleLogin(userID)
		return
	}

	// Route all other messages through the unified gateway.
	if b.gateway != nil {
		asyncSend := func(msg string) { b.sendText(context.Background(), userID, msg) }
		reset := func() { b.convManager.Complete(chatID) }
		if reply, _, ok := b.gateway.Handle(content, asyncSend, reset); ok {
			b.sendText(ctx, userID, reply)
			return
		}
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
		if botcommon.IsConfirmation(content) {
			b.handleConfirmation(userID, chatID, conv)
			return
		}
		if botcommon.IsDenial(content) {
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

	sessionID, err := b.starter.StartAgent(message, "wechat")
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
			b.sendOutputFiles(context.Background(), userID, session.OutputFiles)
		}
		if b.analyzer != nil && conv.NeedsCompaction() {
			b.summarizeConversation(conv)
		}

		if hasPending {
			conv.SetState(conversation.StateGathering)
			b.sendText(context.Background(), userID, "Processing queued messages...")
			b.handleAnalysis(userID, chatID, conv)
		}
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

// wechatReporter adapts botcommon.PollAndReport's callbacks to WeChat
// message sends. WeChat reports progress per completed iteration, so
// OnIterationStart is unused. Every send uses a fresh context.Background(),
// matching this poll loop's previous behavior (WeChat's context-token
// routing doesn't carry across a parent request's cancellation).
type wechatReporter struct {
	bot    *Bot
	userID string
}

func (r *wechatReporter) OnIterationComplete(iter agent.IterationResult) {
	r.bot.sendText(context.Background(), r.userID, formatIteration(iter))
}
func (r *wechatReporter) OnIterationStart(current, max int) {}
func (r *wechatReporter) OnFinal(session *agent.Session) {
	r.bot.sendText(context.Background(), r.userID, formatFinalResult(session))
}
func (r *wechatReporter) OnNotFound() {
	r.bot.sendText(context.Background(), r.userID, "Session not found.")
}
func (r *wechatReporter) OnTimeout() {
	r.bot.sendText(context.Background(), r.userID, "Session timed out waiting for a response.")
}

// pollAndReport polls the agent session every 5 seconds and sends incremental
// iteration updates to the user.
func (b *Bot) pollAndReport(userID, sessionID string) {
	botcommon.PollAndReport(b.starter, sessionID, &wechatReporter{bot: b, userID: userID})
}

// sendText sends a plain-text message to a WeChat user.
func (b *Bot) sendText(ctx context.Context, userID, text string) {
	if err := b.sendWithRetry(userID, func(token string) error {
		return b.client.SendMessage(ctx, userID, text, token)
	}); err != nil {
		slog.Error("wechat: failed to send message", "user_id", userID, "error", err)
	}
}

// sendOutputFiles delivers each agent output file to the WeChat user.
// Images are sent via SendImage; other files via SendFile.
func (b *Bot) sendOutputFiles(ctx context.Context, userID string, files []agent.OutputFile) {
	cdnBaseURL := b.downloader.cdnBaseURL

	for _, f := range files {
		if isImageContentType(f.ContentType) {
			uploaded, err := UploadImage(ctx, b.client, cdnBaseURL, userID, f.Data)
			if err != nil {
				slog.Warn("wechat: output image upload failed, falling back to text", "file", f.Name, "error", err)
				b.sendText(ctx, userID, fmt.Sprintf("[Image: %s — upload failed]", f.Name))
				continue
			}
			if err := b.sendWithRetry(userID, func(token string) error {
				return b.client.SendImage(ctx, userID, uploaded.DownloadParam, uploaded.AESKeyHex, token, uploaded.CiphertextSize)
			}); err != nil {
				slog.Error("wechat: failed to send output image", "file", f.Name, "error", err)
			}
		} else {
			uploaded, err := UploadFile(ctx, b.client, cdnBaseURL, userID, f.Data)
			if err != nil {
				slog.Warn("wechat: output file upload failed, falling back to text", "file", f.Name, "error", err)
				b.sendText(ctx, userID, fmt.Sprintf("[File: %s — upload failed]", f.Name))
				continue
			}
			if err := b.sendWithRetry(userID, func(token string) error {
				return b.client.SendFile(ctx, userID, f.Name, uploaded.DownloadParam, uploaded.AESKeyHex, uploaded.RawMD5Hex, token, uploaded.CiphertextSize)
			}); err != nil {
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

// formatIteration formats a single iteration result. Unlike telegram, WeChat
// has no Markdown rendering, so the commit hash isn't backtick-wrapped.
func formatIteration(iter agent.IterationResult) string {
	return botcommon.FormatIterationCore(iter, false)
}

func formatFinalResult(session *agent.Session) string {
	sb := botcommon.FormatStatusLine(session)
	if len(session.OutputFiles) > 0 {
		sb += fmt.Sprintf("\n\n%d file(s) sent", len(session.OutputFiles))
	}
	sb += botcommon.FormatWarningsSuffix(session)
	return sb
}
