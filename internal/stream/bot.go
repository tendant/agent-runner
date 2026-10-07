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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/botcommon"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/task"
	"github.com/agent-runner/agent-runner/internal/thread"
	"github.com/agent-runner/agent-runner/internal/wechat"
	qrcode "github.com/skip2/go-qrcode"
)

// AgentStarter is the interface for starting and polling agent sessions.
type AgentStarter = botcommon.AgentStarter

// Gateway routes incoming messages through command dispatch before any
// conversation or agent logic. It is the single entry point for all messages.
type Gateway = botcommon.Gateway

// Bot bridges agent-stream channels to the agent runner. It listens on each
// configured channel, and every agent-stream thread is its own thread.Thread:
// state machine, history and agent session are all keyed by thread ID, so
// several threads in one channel run independently.
//
// The engine's id is a thread key: a thread root message ID (m_...) or, for
// channel-level sends such as the welcome and notifications, a channel ID
// (c_...). Client methods route on that prefix.
type Bot struct {
	client            *Client
	starter           AgentStarter
	gateway           Gateway
	threadManager     *thread.Manager
	analyzer          *thread.Analyzer
	channelIDs        []string      // fixed channel list; empty = follow the bot's memberships
	discoveryInterval time.Duration // how often to re-list the bot's channels when following them
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

	// Threads are handled concurrently (each in arrival order); the cursor
	// trackers keep a channel's saved cursor behind unfinished messages.
	dispatcher *threadDispatcher
	cursorsMu  sync.Mutex
	cursors    map[string]*cursorTracker

	// listeners holds a cancel func per channel being listened on.
	listenersMu sync.Mutex
	listeners   map[string]context.CancelFunc
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
func New(cfg config.StreamConfig, uploadsDir string, starter AgentStarter, threadMgr *thread.Manager, analyzer *thread.Analyzer, gateway Gateway) *Bot {
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
		threadManager:     threadMgr,
		analyzer:          analyzer,
		channelIDs:        cfg.ChannelIDs,
		discoveryInterval: cfg.DiscoveryInterval,
		uploadsDir:        uploadsDir,
		botUserID:         extractBotUserID(cfg.BotToken),
		pollInterval:      cfg.PollInterval,
		stateDir:          cfg.StateDir,
		maxCatchUpBacklog: maxCatchUpBacklog,
	}
	b.dispatcher = newThreadDispatcher(defaultMaxThreadHandlers, &b.wg)
	b.cursors = make(map[string]*cursorTracker)
	b.engine = &botcommon.Engine{
		Starter:       starter,
		ThreadManager: threadMgr,
		Analyzer:      analyzer,
		Sender:        (*streamSender)(b),
		Source:        "stream",
		Label:         "stream bot",
		StartText:     "Working on it...",
		// The session-started note is logged, not sent — streaming clients
		// see progress through thinking/delta events instead.
		SessionStartedFormat: "",
		AnnounceQueued:       false,
		OnSessionDone: func(ctx context.Context, id string, session *agent.Session) {
			b.uploadOutputFiles(ctx, id, session)
		},
		NewReporter: func(id string) botcommon.Reporter {
			return &streamReporter{bot: b, ctx: context.Background(), key: id}
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
func (b *Bot) NotifyConversation(ctx context.Context, key, text string) {
	b.engine.Sender.Final(ctx, key, text)
}

// ResumeSession re-attaches a result watcher to a recovered session.
func (b *Bot) ResumeSession(key, sessionID string) {
	b.engine.ResumeSession(context.Background(), key, sessionID)
}

// SetTasks enables multi-turn tasks backed by store (nil disables them),
// each capped by limits.
func (b *Bot) SetTasks(store *task.Store, limits task.Limits) {
	b.engine.Tasks = store
	b.engine.TaskLimits = limits
}

// SetWelcome configures the one-time first-contact greeting.
func (b *Bot) SetWelcome(w botcommon.Welcome) {
	b.engine.Welcome = w
}

// Start begins listening. Non-blocking. With STREAM_CHANNEL_IDS set it
// listens on exactly those channels; otherwise it follows the bot's
// memberships — every channel the bot is in, re-listed every
// discoveryInterval, so adding the bot to a channel (or removing it) takes
// effect without a restart.
func (b *Bot) Start(ctx context.Context) error {
	ctx, b.cancel = context.WithCancel(ctx)
	// The server allows one live process per bot. If another process takes
	// over, stop instead of reconnecting against it.
	cancel := b.cancel
	b.client.onReplaced = func() {
		slog.Error("stream bot: another process is now running this bot (bot_instance_replaced); stopping the stream bot here. " +
			"Run one agent-runner per bot: stop the other process, or give each agent its own bot.")
		cancel()
	}

	// The bot recognises Messages addressed to it by its user ID. It is
	// normally read from the token; ask the server when the token doesn't
	// carry it.
	if b.botUserID == "" {
		id, err := b.client.WhoAmI(ctx)
		if err != nil {
			b.cancel()
			return fmt.Errorf("stream bot: cannot determine the bot's user ID: %w", err)
		}
		b.botUserID = id
	}

	if len(b.channelIDs) > 0 {
		for _, channelID := range b.channelIDs {
			b.startListener(ctx, channelID)
		}
		slog.Info("stream bot started", "conversations", b.channelIDs)
		return nil
	}

	interval := b.discoveryInterval
	if interval <= 0 {
		interval = defaultDiscoveryInterval
	}
	b.syncChannels(ctx)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.syncChannels(ctx)
			}
		}
	}()
	slog.Info("stream bot started, following its channels", "interval", interval, "conversations", b.activeChannels())
	return nil
}

// defaultDiscoveryInterval is how often the bot re-lists its channels.
const defaultDiscoveryInterval = 30 * time.Second

// syncChannels starts listeners for channels the bot has joined and stops
// those for channels it has left. A failed listing changes nothing.
func (b *Bot) syncChannels(ctx context.Context) {
	ids, err := b.client.ListChannels(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("stream bot: could not list channels", "error", err)
		}
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
		if b.startListener(ctx, id) {
			slog.Info("stream bot: joined channel", "channel_id", id)
		}
	}
	b.listenersMu.Lock()
	defer b.listenersMu.Unlock()
	for id, cancel := range b.listeners {
		if !want[id] {
			cancel()
			delete(b.listeners, id)
			slog.Info("stream bot: left channel", "channel_id", id)
		}
	}
}

// startListener listens on channelID unless it already is; reports whether
// it started one.
func (b *Bot) startListener(ctx context.Context, channelID string) bool {
	b.listenersMu.Lock()
	defer b.listenersMu.Unlock()
	if b.listeners == nil {
		b.listeners = make(map[string]context.CancelFunc)
	}
	if _, ok := b.listeners[channelID]; ok {
		return false
	}
	lctx, cancel := context.WithCancel(ctx)
	b.listeners[channelID] = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.listenConversation(lctx, channelID)
	}()
	return true
}

// activeChannels returns the channels being listened on, sorted.
func (b *Bot) activeChannels() []string {
	b.listenersMu.Lock()
	defer b.listenersMu.Unlock()
	ids := make([]string, 0, len(b.listeners))
	for id := range b.listeners {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SendNotification sends a message to every channel the bot listens on.
// Intended for external systems (monitoring, cron jobs, etc.) to post messages.
func (b *Bot) SendNotification(ctx context.Context, message string) error {
	var lastErr error
	for _, channelID := range b.activeChannels() {
		if err := b.client.SendMessage(ctx, channelID, message, nil); err != nil {
			slog.Error("stream bot: failed to notify", "channel_id", channelID, "error", err)
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
func (b *Bot) listenConversation(ctx context.Context, channelID string) {
	// Resume from the last persisted cursor if we have one, instead of
	// re-downloading the whole conversation history on every restart. Only
	// fall back to the full catch-up scan when there's genuinely no saved
	// cursor yet (first run, or the state dir was cleared).
	afterSeq, ok := b.loadCursor(channelID)
	if ok {
		slog.Info("stream bot resumed from saved cursor", "channel_id", channelID, "after_seq", afterSeq, "mode", b.mode())
	} else {
		afterSeq = b.catchUpSeq(ctx, channelID)
		slog.Info("stream bot caught up", "channel_id", channelID, "after_seq", afterSeq, "mode", b.mode())
		b.saveCursor(channelID, afterSeq)
	}
	b.newCursor(channelID, afterSeq)

	if b.pollInterval > 0 {
		b.listenPoll(ctx, channelID, afterSeq)
	} else {
		b.listenSSE(ctx, channelID, afterSeq)
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
func (b *Bot) listenPoll(ctx context.Context, channelID string, afterSeq int64) {
	slog.Info("stream bot: polling started", "channel_id", channelID, "interval", b.pollInterval, "after_seq", afterSeq)
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		events, err := b.client.PollEvents(ctx, channelID, afterSeq)
		if err != nil {
			slog.Error("stream bot poll error", "channel_id", channelID, "error", err)
			continue
		}

		sortBySeq(events)
		afterSeq = b.processEventBatch(ctx, channelID, events, afterSeq)
	}
}

// listenSSE connects to the SSE stream and processes events until the connection
// drops, then reconnects. afterSeq acts as the deduplication cursor.
func (b *Bot) listenSSE(ctx context.Context, channelID string, afterSeq int64) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		events, err := b.client.StreamEvents(ctx, channelID, afterSeq)
		if err != nil {
			slog.Error("stream bot: SSE connect error", "channel_id", channelID, "error", err)
			// Permanent errors (auth failure, not found) will never recover —
			// stop retrying immediately so the log isn't flooded.
			if isPermanentSSEError(err) {
				slog.Error("stream bot: permanent error, stopping SSE listener", "channel_id", channelID, "error", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		slog.Info("stream bot: SSE connected", "channel_id", channelID, "after_seq", afterSeq)

		var received int
		afterSeq, received = b.consumeSSEStream(ctx, channelID, events, afterSeq)

		delay := 2 * time.Second
		if received == 0 {
			delay = 15 * time.Second
		}
		slog.Info("stream bot SSE connection closed, reconnecting", "channel_id", channelID, "events_received", received, "delay", delay)
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
func (b *Bot) consumeSSEStream(ctx context.Context, channelID string, events <-chan Event, afterSeq int64) (newAfterSeq int64, received int) {
	newAfterSeq = afterSeq

	var burst []Event
	idle := time.NewTimer(backlogIdleWindow)
	defer idle.Stop()

burstLoop:
	for {
		select {
		case event, ok := <-events:
			if !ok {
				newAfterSeq = b.processEventBatch(ctx, channelID, burst, newAfterSeq)
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
			newAfterSeq = b.processEventBatch(ctx, channelID, burst, newAfterSeq)
			return newAfterSeq, received
		}
	}

	newAfterSeq = b.processEventBatch(ctx, channelID, burst, newAfterSeq)

	for event := range events {
		received++
		if event.Seq <= newAfterSeq {
			continue // already processed (shouldn't happen, but guard it)
		}
		if event.Type == "message.created" {
			b.dispatchMessage(ctx, channelID, event)
		} else {
			b.cursor(channelID).advance(event.Seq)
		}
		newAfterSeq = event.Seq // dispatched; the tracker persists once handled
	}

	return newAfterSeq, received
}

// processEventBatch processes a batch of events (already or about-to-be
// sorted ascending by seq), skipping all but the most recent
// maxCatchUpBacklog "message.created" events so the bot doesn't try to react
// to a large pile of messages that piled up while it was disconnected. The
// cursor still advances past skipped events so they aren't replayed on the
// next reconnect. Returns the advanced cursor.
func (b *Bot) processEventBatch(ctx context.Context, channelID string, events []Event, afterSeq int64) int64 {
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
		if event.Type == "message.created" && seen >= skipTarget {
			b.dispatchMessage(ctx, channelID, event)
		} else {
			if event.Type == "message.created" {
				skipped++
			}
			b.cursor(channelID).advance(event.Seq)
		}
		if event.Type == "message.created" {
			seen++
		}
		afterSeq = event.Seq // dispatched; the tracker persists once handled
	}
	if skipped > 0 {
		slog.Warn("stream bot: skipped stale backlog messages after reconnect gap",
			"channel_id", channelID, "skipped", skipped, "processed", total-skipped)
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
func (b *Bot) catchUpSeq(ctx context.Context, channelID string) int64 {
	events, err := b.client.PollEvents(ctx, channelID, 0)
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
		slog.Warn("stream bot catch-up failed", "channel_id", channelID, "error", err)
		return 0
	}

	// Server doesn't support PollEvents — drain the SSE stream until idle.
	slog.Debug("stream bot catch-up: poll endpoint not available, using SSE idle-drain", "channel_id", channelID)
	return b.catchUpViaSSE(ctx, channelID)
}

// catchUpViaSSE opens an SSE stream from seq=0 and returns the highest seq seen
// once the stream has been idle (no events) for 500ms, or 10s have elapsed.
func (b *Bot) catchUpViaSSE(ctx context.Context, channelID string) int64 {
	const idleTimeout = 500 * time.Millisecond
	const hardCap = 10 * time.Second

	capCtx, cancel := context.WithTimeout(ctx, hardCap)
	defer cancel()

	ch, err := b.client.StreamEvents(capCtx, channelID, 0)
	if err != nil {
		slog.Warn("stream bot catch-up (SSE) failed", "channel_id", channelID, "error", err)
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
func (b *Bot) cursorPath(channelID string) string {
	// Sanitise channelID: keep alphanumeric, dash, underscore; replace rest with _.
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, channelID)
	return filepath.Join(b.stateDir, "stream-cursor-"+safe+".txt")
}

// loadCursor restores the last-persisted event-seq cursor for a conversation.
// ok is false if no cursor has been saved yet (first run) or stateDir is
// unset — callers should fall back to the full catch-up scan in that case.
func (b *Bot) loadCursor(channelID string) (seq int64, ok bool) {
	if b.stateDir == "" {
		return 0, false
	}
	data, err := os.ReadFile(b.cursorPath(channelID))
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
func (b *Bot) saveCursor(channelID string, seq int64) {
	if b.stateDir == "" {
		return
	}
	if err := os.MkdirAll(b.stateDir, 0755); err != nil {
		slog.Warn("stream bot: failed to create state dir", "dir", b.stateDir, "error", err)
		return
	}
	if err := os.WriteFile(b.cursorPath(channelID), []byte(strconv.FormatInt(seq, 10)), 0600); err != nil {
		slog.Warn("stream bot: failed to persist cursor", "channel_id", channelID, "error", err)
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
	MessageID  string `json:"message_id"`
	ThreadID   string `json:"thread_id,omitempty"` // empty for a thread root
	UserID     string `json:"user_id"`
	SenderKind string `json:"sender_kind,omitempty"` // human, bot, webhook, system
	Content    string `json:"content"`
	// Addressees are the bots the server addressed this Message to; a bot
	// acts only on Messages that list it.
	Addressees []string          `json:"addressees"`
	Mentions   []string          `json:"mentions,omitempty"`
	RunID      string            `json:"run_id,omitempty"`
	FileIDs    []string          `json:"file_ids,omitempty"`
	FileURLs   map[string]string `json:"file_urls,omitempty"` // presigned download URLs for files
}

// newCursor starts tracking channelID's cursor from afterSeq.
func (b *Bot) newCursor(channelID string, afterSeq int64) {
	b.cursorsMu.Lock()
	defer b.cursorsMu.Unlock()
	b.cursors[channelID] = newCursorTracker(afterSeq, func(seq int64) { b.saveCursor(channelID, seq) })
}

// cursor returns channelID's tracker, creating one from the saved cursor if
// the channel was not started through listenConversation (e.g. in tests).
func (b *Bot) cursor(channelID string) *cursorTracker {
	b.cursorsMu.Lock()
	defer b.cursorsMu.Unlock()
	t, ok := b.cursors[channelID]
	if !ok {
		start, _ := b.loadCursor(channelID)
		t = newCursorTracker(start, func(seq int64) { b.saveCursor(channelID, seq) })
		b.cursors[channelID] = t
	}
	return t
}

// dispatchMessage queues a message.created event on its thread: messages in
// one thread are handled in order, different threads concurrently.
func (b *Bot) dispatchMessage(ctx context.Context, channelID string, event Event) {
	var msg messagePayload
	_ = json.Unmarshal(event.Payload, &msg) // a bad payload is reported by handleMessageEvent
	tracker := b.cursor(channelID)
	tracker.begin(event.Seq)
	b.dispatcher.Dispatch(threadKey(channelID, msg), func() {
		defer tracker.done(event.Seq)
		b.handleMessageEvent(ctx, channelID, event)
	})
}

func (b *Bot) handleMessageEvent(ctx context.Context, channelID string, event Event) {
	var msg messagePayload
	if err := json.Unmarshal(event.Payload, &msg); err != nil {
		slog.Error("stream bot: failed to parse message payload", "error", err)
		return
	}

	// The server decides who a Message is for (a mention, the thread's
	// assignee, or the channel's default bot); act only when it's this bot.
	if !addressedTo(msg, b.botUserID) {
		return
	}
	key := threadKey(channelID, msg)

	text := strings.TrimSpace(msg.Content)
	slog.Info("stream bot: message received", "channel_id", channelID, "user_id", msg.UserID, "len", len(text), "files", len(msg.FileIDs))

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

	b.handleMessage(ctx, channelID, key, text)
}

// addressedTo reports whether the server addressed msg to botUserID.
func addressedTo(msg messagePayload, botUserID string) bool {
	if botUserID == "" {
		return false
	}
	for _, id := range msg.Addressees {
		if id == botUserID {
			return true
		}
	}
	return false
}

// threadKey returns the thread a message belongs to: its thread_id for a
// reply, its own ID for a thread root, or the channel for servers that
// predate threads.
func threadKey(channelID string, msg messagePayload) string {
	if msg.ThreadID != "" {
		return msg.ThreadID
	}
	if msg.MessageID != "" {
		return msg.MessageID
	}
	return channelID
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

// handleMessage routes one message into its thread's state machine.
func (b *Bot) handleMessage(ctx context.Context, channelID, key, text string) {
	// Greet once per bot, not per channel: users open channels with it all
	// the time. The greeting answers the first message, in its thread.
	b.engine.WelcomeIfNeededTo(ctx, "stream-"+b.botUserID, key)

	// /wechat-login runs a channel-specific QR flow — handle before the gateway.
	if text == "/wechat-login" {
		b.handleWeChatLogin(ctx, key)
		return
	}

	// Route all other messages through the unified gateway.
	if b.gateway != nil {
		asyncSend := func(msg string) { b.emitFinal(ctx, key, msg) }
		reset := func() { b.engine.ResetThread(key) }
		if reply, _, ok := b.gateway.Handle(text, asyncSend, reset); ok {
			b.emitFinal(ctx, key, reply)
			return
		}
	}

	conv := b.threadManager.GetOrCreate(key)
	conv.AddMessage("user", text)

	state := conv.GetState()

	if state == thread.StateExecuting {
		b.engine.HandleExecuting(ctx, key, conv, text)
		return
	}

	// An open task (waiting for an answer, paused, or recently done) gets
	// the message first: an answer, a go-ahead or feedback continues it.
	if state == thread.StateGathering && b.engine.HandleTaskMessage(ctx, key, conv, text) {
		return
	}

	if state == thread.StateConfirming {
		if botcommon.IsConfirmation(text) {
			b.engine.HandleConfirmation(ctx, key, conv)
			return
		}
		if botcommon.IsDenial(text) {
			conv.SetState(thread.StateGathering)
			resp := "OK, what would you like to change?"
			conv.AddMessage("assistant", resp)
			b.emitFinal(ctx, key, resp)
			return
		}
	}

	// If no analyzer is configured, skip analysis and execute directly
	if b.analyzer == nil {
		b.engine.HandleConfirmation(ctx, key, conv)
		return
	}

	b.engine.HandleAnalysis(ctx, key, conv)
}

// uploadOutputFiles uploads output files from the agent session and sends them
// as a message with file attachments. Files that fail to upload are reported
// by name so the user knows they were generated but not delivered.
func (b *Bot) uploadOutputFiles(ctx context.Context, key string, session *agent.Session) {
	var fileIDs []string
	var uploaded []string
	var failed []string

	for _, f := range session.OutputFiles {
		slog.Info("stream bot: uploading file", "file", f.Name, "content_type", f.ContentType, "bytes", len(f.Data))
		fileID, err := b.client.UploadFile(ctx, key, f.Name, f.ContentType, f.Data)
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
		if err := b.client.SendMessage(ctx, key, strings.Join(parts, "\n"), fileIDs); err != nil {
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
	bot *Bot
	ctx context.Context
	key string
}

func (r *streamReporter) OnIterationComplete(iter agent.IterationResult) {}
func (r *streamReporter) OnIterationStart(current, max int) {
	r.bot.emitThinking(r.ctx, r.key, fmt.Sprintf("Iteration %d/%d...", current, max))
}
func (r *streamReporter) OnFinal(session *agent.Session) {
	r.bot.emitFinal(r.ctx, r.key, formatFinalResult(session))
}
func (r *streamReporter) OnNotFound() { r.bot.emitFinal(r.ctx, r.key, "Session not found.") }
func (r *streamReporter) OnTimeout() {
	r.bot.emitFinal(r.ctx, r.key, "Session timed out waiting for a response.")
}

// Event emission helpers

func (b *Bot) emitThinking(ctx context.Context, key, msg string) {
	b.emit(ctx, key, "run.status", map[string]string{"message": msg, "run_id": b.runID(key)})
}

func (b *Bot) emitDelta(ctx context.Context, key, text string) {
	b.emit(ctx, key, "reply.delta", map[string]string{"delta": text, "run_id": b.runID(key)})
}

// emitFinal ends the run with its result Message (run_id set), which is how
// clients close the streaming reply and how other bots see it.
func (b *Bot) emitFinal(ctx context.Context, key, text string) {
	m := OutMessage{Content: text, RunID: b.runID(key), IdempotencyKey: uuid.NewString()}
	b.retrySend(ctx, "result message", func() error { return b.client.PostMessage(ctx, key, m) })
}

// runID names the bot's run in a thread: one at a time per thread, so the
// thread ID; a channel-level send gets the bot's own run.
func (b *Bot) runID(key string) string {
	if strings.HasPrefix(key, "c_") {
		return "u:" + b.botUserID
	}
	return key
}

// emitBackoffs is the inter-attempt delay schedule for send retries.
// Three attempts total → two backoff waits (between 1→2 and 2→3).
var emitBackoffs = []time.Duration{
	250 * time.Millisecond,
	1 * time.Second,
	3 * time.Second,
}

func (b *Bot) emit(ctx context.Context, key, eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("stream bot: marshal error", "error", err)
		return
	}
	b.retrySend(ctx, eventType, func() error { return b.client.EmitEvent(ctx, key, eventType, data) })
}

// retrySend runs send, retrying transient failures (TLS handshake timeouts,
// connection resets, 5xx, …) so a network hiccup doesn't drop a reply.
// Permanent errors (4xx other than 429) fail fast.
func (b *Bot) retrySend(ctx context.Context, what string, send func() error) {
	maxAttempts := len(emitBackoffs) + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := send()
		if err == nil {
			if attempt > 1 {
				slog.Info("stream bot: send succeeded after retry", "what", what, "attempts", attempt)
			}
			return
		}
		lastErr = err
		if !isTransientEmitError(err) || attempt == maxAttempts {
			break
		}
		backoff := emitBackoffs[attempt-1]
		slog.Warn("stream bot: transient send error, retrying",
			"what", what, "attempt", attempt, "next_backoff", backoff, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			slog.Warn("stream bot: send cancelled during retry", "what", what, "error", ctx.Err())
			return
		}
	}
	slog.Error("stream bot: send error", "what", what, "attempts", maxAttempts, "error", lastErr)
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
func (b *Bot) handleWeChatLogin(ctx context.Context, key string) {
	if b.wechatReloader == nil {
		b.emitFinal(ctx, key, "WeChat bot is not configured on this server.")
		return
	}

	if !b.wechatLoginMu.TryLock() {
		b.emitFinal(ctx, key, "A WeChat login is already in progress. Please wait.")
		return
	}

	b.emitFinal(ctx, key, "Starting WeChat login flow...")

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.wechatLoginMu.Unlock()

		send := func(msg string) {
			b.emitFinal(ctx, key, msg)
		}
		sendQR := func(qrCtx context.Context, qrContent string) {
			pngBytes, err := qrcode.Encode(qrContent, qrcode.Medium, 256)
			if err != nil {
				slog.Error("stream: failed to generate qr code image", "error", err)
				send("Tap the link below in WeChat to authorize the bot login:\n\n" + qrContent)
				return
			}
			fileID, err := b.client.UploadFile(qrCtx, key, "qrcode.png", "image/png", pngBytes)
			if err != nil {
				slog.Error("stream: failed to upload qr code image", "error", err)
				send("Tap the link below in WeChat to authorize the bot login:\n\n" + qrContent)
				return
			}
			if err := b.client.SendMessage(qrCtx, key, "Scan the QR code in WeChat to log in:", []string{fileID}); err != nil {
				slog.Error("stream: failed to send qr code message", "error", err)
			}
		}

		result, err := wechat.RunLoginFlow(ctx, b.wechatBaseURL, send, sendQR)
		if err != nil {
			slog.Error("stream: wechat login flow failed", "error", err)
			b.emitFinal(ctx, key, "Login failed: "+err.Error())
			return
		}

		if err := config.SetEnvLocal("WECHAT_TOKEN", result.Token); err != nil {
			slog.Error("stream: failed to save wechat token to .env.local", "error", err)
			b.emitFinal(ctx, key, "Login succeeded but could not save token: "+err.Error())
			return
		}
		if result.BaseURL != "" {
			if err := config.SetEnvLocal("WECHAT_BASE_URL", result.BaseURL); err != nil {
				slog.Warn("stream: failed to save wechat base_url to .env.local", "error", err)
			}
		}

		b.wechatReloader(result.Token, result.BaseURL)
		b.emitFinal(ctx, key, "WeChat login successful! Bot is now active.")
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
