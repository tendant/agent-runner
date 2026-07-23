package stream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
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
	"github.com/agent-runner/agent-runner/internal/wechat"
	qrcode "github.com/skip2/go-qrcode"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter = botcommon.AgentStarter

// Gateway routes incoming messages through command dispatch before any
// conversation or agent logic. It is the single entry point for all messages.
type Gateway = botcommon.Gateway

// Bot bridges agent-stream conversations to the agent runner.
type Bot struct {
	client            *Client
	starter           AgentStarter
	gateway           Gateway
	convManager       *conversation.Manager
	analyzer          *conversation.Analyzer
	convIDs           []string
	botUserID         string
	uploadsDir        string                      // persistent directory for user-uploaded files
	pollInterval      time.Duration               // >0 = poll mode; 0 = SSE mode
	stateDir          string                      // persistent directory for the per-conversation event-seq cursor
	maxCatchUpBacklog int                         // cap on messages reacted to after a reconnect gap; 0 = use default
	wechatReloader    func(token, baseURL string) // called after a successful /wechat-login
	wechatBaseURL     string                      // iLink API base URL for the login flow
	wechatLoginMu     sync.Mutex                  // prevents concurrent /wechat-login flows
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	engine            *botcommon.Engine
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
func New(cfg config.StreamConfig, uploadsDir string, starter AgentStarter, convMgr *conversation.Manager, analyzer *conversation.Analyzer, gateway Gateway) *Bot {
	if cfg.ServerURL == "" || cfg.BotToken == "" {
		return nil
	}

	maxCatchUpBacklog := cfg.MaxCatchUpBacklog
	if maxCatchUpBacklog <= 0 {
		maxCatchUpBacklog = defaultMaxCatchUpBacklog
	}

	b := &Bot{
		client:            NewClient(cfg.ServerURL, cfg.BotToken),
		starter:           starter,
		gateway:           gateway,
		convManager:       convMgr,
		analyzer:          analyzer,
		convIDs:           cfg.ConversationIDs,
		uploadsDir:        uploadsDir,
		botUserID:         extractBotUserID(cfg.BotToken),
		pollInterval:      cfg.PollInterval,
		stateDir:          cfg.StateDir,
		maxCatchUpBacklog: maxCatchUpBacklog,
	}
	b.engine = &botcommon.Engine{
		Starter:     starter,
		ConvManager: convMgr,
		Analyzer:    analyzer,
		Sender:      (*streamSender)(b),
		Source:      "stream",
		Label:       "stream bot",
		StartText:   "Working on it...",
		// The session-started note is logged, not sent — streaming clients
		// see progress through thinking/delta events instead.
		SessionStartedFormat: "",
		AnnounceQueued:       false,
		OnSessionDone: func(ctx context.Context, id string, session *agent.Session) {
			b.uploadOutputFiles(ctx, id, session)
		},
		NewReporter: func(id string) botcommon.Reporter {
			return &streamReporter{bot: b, ctx: context.Background(), convID: id}
		},
		WG: &b.wg,
	}
	return b
}

// streamSender adapts the engine's Sender to typed stream events: Status is
// a thinking event, Reply keeps the delta stream open, Final closes it.
type streamSender Bot

func (s *streamSender) Status(ctx context.Context, id, text string) {
	(*Bot)(s).emitThinking(ctx, id, text)
}
func (s *streamSender) Reply(ctx context.Context, id, text string) {
	(*Bot)(s).emitDelta(ctx, id, text+"\n")
}
func (s *streamSender) Final(ctx context.Context, id, text string) {
	(*Bot)(s).emitFinal(ctx, id, text)
}

// NotifyConversation sends a message to a specific conversation. Used by
// restart recovery to reach the session's originating chat.
func (b *Bot) NotifyConversation(ctx context.Context, convID, text string) {
	b.engine.Sender.Final(ctx, convID, text)
}

// ResumeSession re-attaches a result watcher to a recovered session.
func (b *Bot) ResumeSession(convID, sessionID string) {
	b.engine.ResumeSession(context.Background(), convID, sessionID)
}

// SetWelcome configures the one-time first-contact greeting.
func (b *Bot) SetWelcome(w botcommon.Welcome) {
	b.engine.Welcome = w
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

	slog.Info("stream bot started", "conversations", b.convIDs)
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

// listenConversation receives events for a single conversation.
// Uses polling (GET /events?after_seq=N) when b.pollInterval > 0, otherwise SSE.
func (b *Bot) listenConversation(ctx context.Context, convID string) {
	// Resume from the last persisted cursor if we have one, instead of
	// re-downloading the whole conversation history on every restart. Only
	// fall back to the full catch-up scan when there's genuinely no saved
	// cursor yet (first run, or the state dir was cleared).
	afterSeq, ok := b.loadCursor(convID)
	if ok {
		slog.Info("stream bot resumed from saved cursor", "conversation_id", convID, "after_seq", afterSeq, "mode", b.mode())
	} else {
		afterSeq = b.catchUpSeq(ctx, convID)
		slog.Info("stream bot caught up", "conversation_id", convID, "after_seq", afterSeq, "mode", b.mode())
		b.saveCursor(convID, afterSeq)
	}

	if b.pollInterval > 0 {
		b.listenPoll(ctx, convID, afterSeq)
	} else {
		b.listenSSE(ctx, convID, afterSeq)
	}
}

func (b *Bot) mode() string {
	if b.pollInterval > 0 {
		return "poll"
	}
	return "sse"
}

// listenPoll polls GET /events?after_seq=N on a fixed interval.
// Events are sorted by seq before processing so out-of-order delivery from the
// server doesn't cause gaps. afterSeq advances after each event is handled.
func (b *Bot) listenPoll(ctx context.Context, convID string, afterSeq int64) {
	slog.Info("stream bot: polling started", "conversation_id", convID, "interval", b.pollInterval, "after_seq", afterSeq)
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		events, err := b.client.PollEvents(ctx, convID, afterSeq)
		if err != nil {
			slog.Error("stream bot poll error", "conversation_id", convID, "error", err)
			continue
		}

		sortBySeq(events)
		afterSeq = b.processEventBatch(ctx, convID, events, afterSeq)
	}
}

// listenSSE connects to the SSE stream and processes events until the connection
// drops, then reconnects. afterSeq acts as the deduplication cursor.
func (b *Bot) listenSSE(ctx context.Context, convID string, afterSeq int64) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		events, err := b.client.StreamEvents(ctx, convID, afterSeq)
		if err != nil {
			slog.Error("stream bot: SSE connect error", "conversation_id", convID, "error", err)
			// Permanent errors (auth failure, not found) will never recover —
			// stop retrying immediately so the log isn't flooded.
			if isPermanentSSEError(err) {
				slog.Error("stream bot: permanent error, stopping SSE listener", "conversation_id", convID, "error", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		slog.Info("stream bot: SSE connected", "conversation_id", convID, "after_seq", afterSeq)

		var received int
		afterSeq, received = b.consumeSSEStream(ctx, convID, events, afterSeq)

		delay := 2 * time.Second
		if received == 0 {
			delay = 15 * time.Second
		}
		slog.Info("stream bot SSE connection closed, reconnecting", "conversation_id", convID, "events_received", received, "delay", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// defaultMaxCatchUpBacklog is the fallback for maxCatchUpBacklog when no
// STREAM_MAX_CATCHUP_BACKLOG is configured.
const defaultMaxCatchUpBacklog = 100

// backlogIdleWindow is how long to wait, after the SSE stream (re)connects,
// for the initial replay burst to go quiet before switching to normal
// per-event live handling. The agent-stream server replays the entire
// history after afterSeq on every connect with no way to ask for less, so
// this idle-based heuristic is how the client tells "replayed backlog" apart
// from "live events" on the same channel.
const backlogIdleWindow = 500 * time.Millisecond

// consumeSSEStream buffers the initial replay burst on a freshly (re)opened
// SSE connection, caps how many of it are actually reacted to (via
// processEventBatch), then processes subsequent live events one at a time as
// they arrive. Returns the advanced cursor and the total number of events
// received (used by the caller to size the reconnect delay).
func (b *Bot) consumeSSEStream(ctx context.Context, convID string, events <-chan Event, afterSeq int64) (newAfterSeq int64, received int) {
	newAfterSeq = afterSeq

	var burst []Event
	idle := time.NewTimer(backlogIdleWindow)
	defer idle.Stop()

burstLoop:
	for {
		select {
		case event, ok := <-events:
			if !ok {
				newAfterSeq = b.processEventBatch(ctx, convID, burst, newAfterSeq)
				return newAfterSeq, received
			}
			received++
			burst = append(burst, event)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(backlogIdleWindow)
		case <-idle.C:
			break burstLoop
		case <-ctx.Done():
			newAfterSeq = b.processEventBatch(ctx, convID, burst, newAfterSeq)
			return newAfterSeq, received
		}
	}

	newAfterSeq = b.processEventBatch(ctx, convID, burst, newAfterSeq)

	for event := range events {
		received++
		if event.Seq <= newAfterSeq {
			continue // already processed (shouldn't happen, but guard it)
		}
		if event.Type == "message.created" {
			b.handleMessageEvent(ctx, convID, event)
		}
		newAfterSeq = event.Seq // advance only after successful handling
		b.saveCursor(convID, newAfterSeq)
	}

	return newAfterSeq, received
}

// processEventBatch processes a batch of events (already or about-to-be
// sorted ascending by seq), skipping all but the most recent
// maxCatchUpBacklog "message.created" events so the bot doesn't try to react
// to a large pile of messages that piled up while it was disconnected. The
// cursor still advances past skipped events so they aren't replayed on the
// next reconnect. Returns the advanced cursor.
func (b *Bot) processEventBatch(ctx context.Context, convID string, events []Event, afterSeq int64) int64 {
	if len(events) == 0 {
		return afterSeq
	}
	sortBySeq(events)

	total := 0
	for _, e := range events {
		if e.Seq > afterSeq && e.Type == "message.created" {
			total++
		}
	}
	skipTarget := 0
	if total > b.maxCatchUpBacklog {
		skipTarget = total - b.maxCatchUpBacklog
	}

	skipped, seen := 0, 0
	for _, event := range events {
		if event.Seq <= afterSeq {
			continue // already processed
		}
		if event.Type == "message.created" {
			if seen < skipTarget {
				skipped++
			} else {
				b.handleMessageEvent(ctx, convID, event)
			}
			seen++
		}
		afterSeq = event.Seq // advance only after successful handling
		b.saveCursor(convID, afterSeq)
	}
	if skipped > 0 {
		slog.Warn("stream bot: skipped stale backlog messages after reconnect gap",
			"conversation_id", convID, "skipped", skipped, "processed", total-skipped)
	}
	return afterSeq
}

// catchUpSeq returns the highest seq currently in the conversation so the bot
// starts from "now" and skips existing history.
//
// Strategy:
//  1. Try PollEvents (single HTTP GET, fast) — works on servers that support it.
//  2. On 404 (server doesn't have the polling endpoint), fall back to an SSE
//     idle-drain: open the stream from seq=0 and close it once 500ms pass with
//     no new events — the silence signals that the history burst is done.
//     A hard cap of 10s prevents hanging on very large histories.
func (b *Bot) catchUpSeq(ctx context.Context, convID string) int64 {
	events, err := b.client.PollEvents(ctx, convID, 0)
	if err == nil {
		var maxSeq int64
		for _, e := range events {
			if e.Seq > maxSeq {
				maxSeq = e.Seq
			}
		}
		return maxSeq
	}

	if err != ErrNotFound {
		slog.Warn("stream bot catch-up failed", "conversation_id", convID, "error", err)
		return 0
	}

	// Server doesn't support PollEvents — drain the SSE stream until idle.
	slog.Debug("stream bot catch-up: poll endpoint not available, using SSE idle-drain", "conversation_id", convID)
	return b.catchUpViaSSE(ctx, convID)
}

// catchUpViaSSE opens an SSE stream from seq=0 and returns the highest seq seen
// once the stream has been idle (no events) for 500ms, or 10s have elapsed.
func (b *Bot) catchUpViaSSE(ctx context.Context, convID string) int64 {
	const idleTimeout = 500 * time.Millisecond
	const hardCap = 10 * time.Second

	capCtx, cancel := context.WithTimeout(ctx, hardCap)
	defer cancel()

	ch, err := b.client.StreamEvents(capCtx, convID, 0)
	if err != nil {
		slog.Warn("stream bot catch-up (SSE) failed", "conversation_id", convID, "error", err)
		return 0
	}

	var maxSeq int64
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()

	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return maxSeq
			}
			if event.Seq > maxSeq {
				maxSeq = event.Seq
			}
			// Reset idle timer on every event.
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleTimeout)
		case <-idle.C:
			// No events for 500ms — history burst is done.
			cancel()
			// Drain the channel so the SSE goroutine can exit.
			for range ch {
			}
			return maxSeq
		case <-capCtx.Done():
			return maxSeq
		}
	}
}

// cursorPath returns the persisted event-seq cursor file path for a conversation.
func (b *Bot) cursorPath(convID string) string {
	// Sanitise convID: keep alphanumeric, dash, underscore; replace rest with _.
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, convID)
	return filepath.Join(b.stateDir, "stream-cursor-"+safe+".txt")
}

// loadCursor restores the last-persisted event-seq cursor for a conversation.
// ok is false if no cursor has been saved yet (first run) or stateDir is
// unset — callers should fall back to the full catch-up scan in that case.
func (b *Bot) loadCursor(convID string) (seq int64, ok bool) {
	if b.stateDir == "" {
		return 0, false
	}
	data, err := os.ReadFile(b.cursorPath(convID))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// saveCursor persists the event-seq cursor for a conversation so the bot can
// resume without a full catch-up scan on the next restart. Best-effort —
// failures are logged, not fatal, since the worst case is falling back to
// catchUpSeq next time.
func (b *Bot) saveCursor(convID string, seq int64) {
	if b.stateDir == "" {
		return
	}
	if err := os.MkdirAll(b.stateDir, 0755); err != nil {
		slog.Warn("stream bot: failed to create state dir", "dir", b.stateDir, "error", err)
		return
	}
	if err := os.WriteFile(b.cursorPath(convID), []byte(strconv.FormatInt(seq, 10)), 0600); err != nil {
		slog.Warn("stream bot: failed to persist cursor", "conversation_id", convID, "error", err)
	}
}

// sortBySeq sorts events ascending by seq so they are processed in order
// regardless of server delivery order.
func sortBySeq(events []Event) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j].Seq < events[j-1].Seq; j-- {
			events[j], events[j-1] = events[j-1], events[j]
		}
	}
}

// messagePayload is the shape of a message.created event payload.
type messagePayload struct {
	UserID   string            `json:"user_id"`
	Content  string            `json:"content"`
	FileIDs  []string          `json:"file_ids,omitempty"`
	FileURLs map[string]string `json:"file_urls,omitempty"` // presigned download URLs for files
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
	slog.Info("stream bot: message received", "conversation_id", convID, "user_id", msg.UserID, "len", len(text), "files", len(msg.FileIDs))

	// Download and inline any attached files
	if len(msg.FileIDs) > 0 {
		fileContent := b.resolveFiles(ctx, msg.FileIDs, msg.FileURLs)
		if fileContent != "" {
			if text != "" {
				text = text + "\n\n" + fileContent
			} else {
				// Images only, no text — prepend a hint so the analyzer
				// treats this as a conversational "ask" (describe/analyze)
				// rather than routing to the agent CLI.
				text = "[User sent images with no text. Analyze and describe them.]\n\n" + fileContent
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
// Uses presigned URLs if available, otherwise falls back to authenticated download.
func (b *Bot) resolveFiles(ctx context.Context, fileIDs []string, fileURLs map[string]string) string {
	var parts []string

	for _, fileID := range fileIDs {
		var file *DownloadedFile
		var err error

		// Use presigned URL if available, otherwise fall back to authenticated download
		if downloadURL, ok := fileURLs[fileID]; ok {
			slog.Info("stream bot: downloading file from presigned URL", "file_id", fileID)
			file, err = b.downloadFileFromURL(ctx, downloadURL)
		} else {
			slog.Info("stream bot: downloading file via authenticated endpoint", "file_id", fileID)
			file, err = b.client.DownloadFile(ctx, fileID)
		}

		if err != nil {
			slog.Error("stream bot: failed to download file", "file_id", fileID, "error", err)
			continue
		}

		if isTextContent(file.ContentType) {
			parts = append(parts, fmt.Sprintf("--- File: %s ---\n%s\n--- End: %s ---", file.Filename, string(file.Data), file.Filename))
		} else {
			// Save binary file to temp dir so the agent can access it
			path, err := b.saveFile(file)
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

// downloadFileFromURL downloads a file from a presigned URL (no authentication needed).
func (b *Bot) downloadFileFromURL(ctx context.Context, presignedURL string) (*DownloadedFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, presignedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := b.client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download file: status %d: %s", resp.StatusCode, string(body))
	}

	// Limit download to 10MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}

	// Extract filename from Content-Disposition header or use file ID
	filename := "file"
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				filename = fn
			}
		}
	}

	return &DownloadedFile{
		Filename:    filename,
		ContentType: resp.Header.Get("Content-Type"),
		Data:        data,
	}, nil
}

// saveFile writes a downloaded file to uploadsDir (persistent) and returns the path.
// Falls back to the system temp dir if uploadsDir is empty.
func (b *Bot) saveFile(file *DownloadedFile) (string, error) {
	dir := b.uploadsDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "agent-runner-files")
	} else {
		dir = filepath.Join(dir, time.Now().UTC().Format("2006-01-02"))
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}
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
	b.engine.WelcomeIfNeeded(ctx, convID)

	// /wechat-login runs a channel-specific QR flow — handle before the gateway.
	if text == "/wechat-login" {
		b.handleWeChatLogin(ctx, convID)
		return
	}

	// Route all other messages through the unified gateway.
	if b.gateway != nil {
		asyncSend := func(msg string) { b.emitFinal(ctx, convID, msg) }
		reset := func() { b.convManager.Complete(convID) }
		if reply, _, ok := b.gateway.Handle(text, asyncSend, reset); ok {
			b.emitFinal(ctx, convID, reply)
			return
		}
	}

	conv := b.convManager.GetOrCreate(convID)
	conv.AddMessage("user", text)

	state := conv.GetState()

	if state == conversation.StateExecuting {
		b.emitFinal(ctx, convID, "Message queued — I'll process it after the current task finishes.")
		return
	}

	if state == conversation.StateConfirming {
		if botcommon.IsConfirmation(text) {
			b.engine.HandleConfirmation(ctx, convID, conv)
			return
		}
		if botcommon.IsDenial(text) {
			conv.SetState(conversation.StateGathering)
			resp := "OK, what would you like to change?"
			conv.AddMessage("assistant", resp)
			b.emitFinal(ctx, convID, resp)
			return
		}
	}

	// If no analyzer is configured, skip analysis and execute directly
	if b.analyzer == nil {
		b.engine.HandleConfirmation(ctx, convID, conv)
		return
	}

	b.engine.HandleAnalysis(ctx, convID, conv)
}

// uploadOutputFiles uploads output files from the agent session and sends them
// as a message with file attachments. Files that fail to upload are reported
// by name so the user knows they were generated but not delivered.
func (b *Bot) uploadOutputFiles(ctx context.Context, convID string, session *agent.Session) {
	var fileIDs []string
	var uploaded []string
	var failed []string

	for _, f := range session.OutputFiles {
		slog.Info("stream bot: uploading file", "file", f.Name, "content_type", f.ContentType, "bytes", len(f.Data))
		fileID, err := b.client.UploadFile(ctx, convID, f.Name, f.ContentType, f.Data)
		if err != nil {
			slog.Error("stream bot: failed to upload file", "file", f.Name, "bytes", len(f.Data), "error", err)
			failed = append(failed, f.Name)
			continue
		}
		slog.Info("stream bot: uploaded file", "file", f.Name, "file_id", fileID)
		fileIDs = append(fileIDs, fileID)
		uploaded = append(uploaded, f.Name)
	}

	var parts []string
	if len(uploaded) > 0 {
		parts = append(parts, fmt.Sprintf("Generated %d file(s): %s", len(uploaded), strings.Join(uploaded, ", ")))
	}
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("Could not deliver %d file(s) (upload failed): %s", len(failed), strings.Join(failed, ", ")))
	}
	if len(parts) > 0 {
		if err := b.client.SendMessage(ctx, convID, strings.Join(parts, "\n"), fileIDs); err != nil {
			slog.Error("stream bot: failed to send message with files", "error", err)
		}
	}
}

// streamReporter adapts botcommon.PollAndReport's callbacks to stream SSE
// events. Stream reports progress via a synthetic "thinking" event per
// started iteration rather than per-completed-iteration text, so
// OnIterationComplete is unused. ctx is a background context, matching this
// poll loop's previous behavior of not tying its sends to any parent
// request's cancellation.
type streamReporter struct {
	bot    *Bot
	ctx    context.Context
	convID string
}

func (r *streamReporter) OnIterationComplete(iter agent.IterationResult) {}
func (r *streamReporter) OnIterationStart(current, max int) {
	r.bot.emitThinking(r.ctx, r.convID, fmt.Sprintf("Iteration %d/%d...", current, max))
}
func (r *streamReporter) OnFinal(session *agent.Session) {
	r.bot.emitFinal(r.ctx, r.convID, formatFinalResult(session))
}
func (r *streamReporter) OnNotFound() { r.bot.emitFinal(r.ctx, r.convID, "Session not found.") }
func (r *streamReporter) OnTimeout() {
	r.bot.emitFinal(r.ctx, r.convID, "Session timed out waiting for a response.")
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

// emitBackoffs is the inter-attempt delay schedule for emit retries.
// Three attempts total → two backoff waits (between 1→2 and 2→3).
var emitBackoffs = []time.Duration{
	250 * time.Millisecond,
	1 * time.Second,
	3 * time.Second,
}

func (b *Bot) emit(ctx context.Context, convID, eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("stream bot: marshal error", "error", err)
		return
	}

	// Retry transient emit failures (TLS handshake timeouts, connection
	// resets, 5xx, etc.) so a Cloudflare hiccup doesn't drop assistant.final
	// on the floor. Permanent errors (4xx other than 429, bad payload) fail fast.
	maxAttempts := len(emitBackoffs) + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		emitErr := b.client.EmitEvent(ctx, convID, eventType, data)
		if emitErr == nil {
			if attempt > 1 {
				slog.Info("stream bot: emit succeeded after retry",
					"event_type", eventType, "attempts", attempt)
			}
			return
		}
		lastErr = emitErr
		if !isTransientEmitError(emitErr) || attempt == maxAttempts {
			break
		}
		backoff := emitBackoffs[attempt-1]
		slog.Warn("stream bot: transient emit error, retrying",
			"event_type", eventType, "attempt", attempt,
			"next_backoff", backoff, "error", emitErr)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			slog.Warn("stream bot: emit cancelled during retry",
				"event_type", eventType, "error", ctx.Err())
			return
		}
	}
	slog.Error("stream bot: emit error",
		"event_type", eventType, "attempts", maxAttempts, "error", lastErr)
}

// isTransientEmitError reports whether an EmitEvent error is worth retrying.
// Covers TLS handshake / i/o timeouts, connection refused / reset, brief DNS
// failures, EOF, generic net.Error timeouts, HTTP 429, and HTTP 5xx.
// Permanent failures (4xx other than 429, payload errors) return false.
func isTransientEmitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{
		"TLS handshake timeout",
		"i/o timeout",
		"connection refused",
		"connection reset",
		"no such host",
		"EOF",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	// HTTP status errors are formatted as "emit event: status N: <body>".
	const statusPrefix = "emit event: status "
	if idx := strings.Index(msg, statusPrefix); idx >= 0 {
		rest := msg[idx+len(statusPrefix):]
		if end := strings.IndexByte(rest, ':'); end > 0 {
			if code, convErr := strconv.Atoi(rest[:end]); convErr == nil {
				if code == 429 || (code >= 500 && code < 600) {
					return true
				}
			}
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// Formatting helpers

// maxLastOutputChars caps the last-iteration output preview prepended to the
// final result — stream's only channel for iteration content, since it
// reports progress via synthetic "thinking" events rather than per-iteration
// text (see pollAndReport / streamReporter).
const maxLastOutputChars = 4000

func formatFinalResult(session *agent.Session) string {
	var sb strings.Builder

	// Include the last iteration's output so the user sees Claude's response
	if len(session.Iterations) > 0 {
		lastOutput := session.Iterations[len(session.Iterations)-1].Output
		if lastOutput != "" {
			if len(lastOutput) > maxLastOutputChars {
				lastOutput = lastOutput[:maxLastOutputChars] + "\n... (truncated)"
			}
			sb.WriteString(lastOutput)
			sb.WriteString("\n\n---\n")
		}
	}

	sb.WriteString(botcommon.FormatStatusLine(session))
	sb.WriteString(botcommon.FormatWarningsSuffix(session))
	return sb.String()
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

// isPermanentSSEError reports whether an SSE connection error is permanent
// (will never succeed on retry). HTTP 401 and 403 are auth failures that
// won't change without a config fix; 404 means the conversation is gone.
func isPermanentSSEError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range []string{"status 401", "status 403", "status 404"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}
