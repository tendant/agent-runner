package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State represents the current phase of a thread.
type State string

const (
	StateGathering  State = "gathering"  // collecting information
	StateConfirming State = "confirming" // plan shown, awaiting yes/no
	StateExecuting  State = "executing"  // agent running
	StateCompleted  State = "completed"
)

// Message is a single message in the thread history.
type Message struct {
	Role    string    `json:"role"` // "user" or "assistant"
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
}

// Thread tracks one unit of a user's work with the bot: its history, state
// machine and active agent session. ChatID is the thread key: an agent-stream
// thread ID, or a whole Telegram/WeChat chat (one thread per chat).
type Thread struct {
	mu sync.Mutex

	ID           string
	ChatID       string
	State        State
	Messages     []Message
	Plan         string // generated plan text
	pendingInput bool   // true if user sent messages during execution

	// activeSessionID is the agent session currently executing for this
	// thread ("" when idle) — used to steer the live run.
	activeSessionID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const maxMessages = 20

// Summarizer can condense thread history into a short summary.
type Summarizer interface {
	Summarize(ctx context.Context, messages []Message) (string, error)
}

// AddMessage appends a message to the thread and updates the timestamp.
// When messages exceed maxMessages, old messages are compacted: if a Summarizer
// is set, they are summarized; otherwise the oldest are dropped.
func (c *Thread) AddMessage(role, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Messages = append(c.Messages, Message{
		Role:    role,
		Content: content,
		Time:    time.Now(),
	})
	if len(c.Messages) > maxMessages {
		c.compact()
	}
	if role == "user" && c.State == StateExecuting {
		c.pendingInput = true
	}
	c.UpdatedAt = time.Now()
}

// compact reduces messages when over the limit. If a summary already exists,
// just drop the oldest non-summary messages. The actual summarization is
// triggered externally via CompactWithSummary to avoid blocking AddMessage.
func (c *Thread) compact() {
	if len(c.Messages) <= maxMessages {
		return
	}
	// Keep the last maxMessages messages, preserving any leading summary
	c.Messages = c.Messages[len(c.Messages)-maxMessages:]
}

// NeedsCompaction returns true if the thread has enough messages to
// benefit from summarization (called before triggering async summarization).
func (c *Thread) NeedsCompaction() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Messages) >= maxMessages-2 // trigger before we hit the hard cap
}

// CompactWithSummary replaces old messages with a summary message, keeping
// the most recent keepRecent messages intact. Thread-safe.
func (c *Thread) CompactWithSummary(summary string, keepRecent int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Messages) <= keepRecent+1 {
		return
	}
	summaryMsg := Message{
		Role:    "assistant",
		Content: "[Conversation summary]\n" + summary,
		Time:    c.Messages[0].Time,
	}
	recent := make([]Message, keepRecent)
	copy(recent, c.Messages[len(c.Messages)-keepRecent:])
	c.Messages = append([]Message{summaryMsg}, recent...)
}

// SetState changes the thread state.
func (c *Thread) SetState(state State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.State = state
	c.UpdatedAt = time.Now()
}

// GetState returns the current state.
func (c *Thread) GetState() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.State
}

// GetMessages returns a copy of the message history.
func (c *Thread) GetMessages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	msgs := make([]Message, len(c.Messages))
	copy(msgs, c.Messages)
	return msgs
}

// SetPlan stores the generated plan text and transitions to confirming state.
func (c *Thread) SetPlan(plan string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Plan = plan
	c.State = StateConfirming
	c.UpdatedAt = time.Now()
}

// GetPlan returns the stored plan text.
func (c *Thread) GetPlan() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Plan
}

// GetUserMessage returns the concatenation of all user messages in the thread.
func (c *Thread) GetUserMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	for _, msg := range c.Messages {
		if msg.Role == "user" {
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// Reset transitions a completed thread back to gathering state,
// preserving message history while clearing the plan.
func (c *Thread) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Plan = ""
	c.pendingInput = false
	c.State = StateGathering
	c.UpdatedAt = time.Now()
}

// SetActiveSession records the agent session executing for this
// thread; pass "" when the run ends.
func (c *Thread) SetActiveSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeSessionID = sessionID
}

// ActiveSessionID returns the executing agent session ID, or "".
func (c *Thread) ActiveSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeSessionID
}

// ClearPendingInput atomically checks and clears the pending input flag.
// Returns true if user messages were queued during execution.
func (c *Thread) ClearPendingInput() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.pendingInput
	c.pendingInput = false
	return had
}

// GetFormattedHistory returns the full thread history formatted for
// inclusion in a prompt. Returns empty string if there are fewer than 2 messages
// (i.e., only the current message exists, so no prior context).
func (c *Thread) GetFormattedHistory() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Only the current message — no history to include
	if len(c.Messages) <= 1 {
		return ""
	}

	var sb strings.Builder
	// Include all messages except the last one (which is the current request)
	for _, msg := range c.Messages[:len(c.Messages)-1] {
		switch msg.Role {
		case "user":
			sb.WriteString("User: ")
		case "assistant":
			sb.WriteString("Assistant: ")
		}
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

const threadIdleTimeout = 30 * time.Minute

// Manager manages active threads keyed by thread key (see Thread.ChatID).
// Persisted files keep the conv- prefix so existing state loads after upgrade.
type Manager struct {
	mu      sync.RWMutex
	threads map[string]*Thread
	nextID  int
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when the cleanup loop has exited
	dir     string        // persistence directory; "" = disabled
}

// NewManager creates a new thread manager. dir is the directory used to
// persist threads to disk across restarts; pass "" to disable persistence.
func NewManager(dir string) *Manager {
	m := &Manager{
		threads: make(map[string]*Thread),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		dir:     dir,
	}
	if dir != "" {
		m.loadAll()
	}
	go m.cleanupLoop()
	return m
}

// convFilePath returns the JSON file path for a thread.
func (m *Manager) convFilePath(chatID string) string {
	// Sanitise chatID: keep alphanumeric, dash, underscore; replace rest with _.
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, chatID)
	return filepath.Join(m.dir, "conv-"+safe+".json")
}

// persist writes a thread to disk. No-op if persistence is disabled.
func (m *Manager) persist(conv *Thread) {
	if m.dir == "" {
		return
	}
	conv.mu.Lock()
	data, err := json.Marshal(conv)
	conv.mu.Unlock()
	if err != nil {
		slog.Warn("thread: marshal failed", "chat_id", conv.ChatID, "error", err)
		return
	}
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		slog.Warn("thread: persist failed to create dir", "dir", m.dir, "error", err)
		return
	}
	// Write-then-rename so concurrent writers (saveAll + background loop)
	// and restart-time readers never see a truncated file.
	tmp, err := os.CreateTemp(m.dir, "conv-*.tmp")
	if err != nil {
		slog.Warn("thread: persist failed", "chat_id", conv.ChatID, "error", err)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		slog.Warn("thread: persist failed", "chat_id", conv.ChatID, "error", err)
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), m.convFilePath(conv.ChatID)); err != nil {
		os.Remove(tmp.Name())
		slog.Warn("thread: persist failed", "chat_id", conv.ChatID, "error", err)
	}
}

// loadAll reads all persisted threads from disk at startup.
func (m *Manager) loadAll() {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		slog.Warn("thread: failed to create persistence dir", "dir", m.dir, "error", err)
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		slog.Warn("thread: failed to read persistence dir", "dir", m.dir, "error", err)
		return
	}
	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "conv-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("thread: failed to read file", "path", path, "error", err)
			continue
		}
		var conv Thread
		if err := json.Unmarshal(data, &conv); err != nil {
			slog.Warn("thread: corrupt file deleted", "path", path, "error", err)
			os.Remove(path)
			continue
		}
		if conv.State == StateCompleted {
			continue // skip completed threads
		}
		if conv.ChatID == "" {
			continue
		}
		// A thread persisted as executing belonged to a session that
		// died with the previous process — left as-is it would queue every
		// new message forever ("Message queued…"). Downgrade so the chat
		// accepts input again; session recovery notifies the user separately.
		if conv.State == StateExecuting {
			slog.Info("thread: resetting executing conversation from previous run", "chat_id", conv.ChatID)
			conv.State = StateGathering
		}
		m.threads[conv.ChatID] = &conv
		loaded++
	}
	if loaded > 0 {
		slog.Info("thread: loaded persisted threads", "count", loaded)
	}
}

// saveAll persists all active threads. Called from the background loop.
func (m *Manager) saveAll() {
	if m.dir == "" {
		return
	}
	m.mu.RLock()
	convs := make([]*Thread, 0, len(m.threads))
	for _, conv := range m.threads {
		convs = append(convs, conv)
	}
	m.mu.RUnlock()
	for _, conv := range convs {
		m.persist(conv)
	}
}

// Stop stops the cleanup loop and waits for its shutdown flush to finish,
// so no background write races whatever the caller does next (e.g. removing
// the persistence dir). Safe to call multiple times.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	<-m.doneCh
}

// cleanupLoop periodically evicts stale threads and saves active ones.
func (m *Manager) cleanupLoop() {
	defer close(m.doneCh)
	evictTicker := time.NewTicker(time.Minute)
	saveTicker := time.NewTicker(30 * time.Second)
	defer evictTicker.Stop()
	defer saveTicker.Stop()

	for {
		select {
		case <-m.stopCh:
			m.saveAll() // flush on shutdown
			return
		case <-evictTicker.C:
			m.evictStale()
		case <-saveTicker.C:
			m.saveAll()
		}
	}
}

func (m *Manager) evictStale() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-threadIdleTimeout)
	for chatID, conv := range m.threads {
		conv.mu.Lock()
		updatedAt := conv.UpdatedAt
		conv.mu.Unlock()

		if updatedAt.Before(cutoff) {
			delete(m.threads, chatID)
			if m.dir != "" {
				os.Remove(m.convFilePath(chatID))
			}
		}
	}
}

// GetOrCreate returns the active thread for a chat, creating one if none exists.
// If the previous thread is completed, it resets it to gathering state while
// preserving message history so the agent has context from prior sessions.
func (m *Manager) GetOrCreate(chatID string) *Thread {
	m.mu.Lock()
	defer m.mu.Unlock()

	if conv, ok := m.threads[chatID]; ok {
		if conv.GetState() == StateCompleted {
			conv.Reset()
		}
		return conv
	}

	m.nextID++
	conv := &Thread{
		ID:        fmt.Sprintf("conv-%d", m.nextID),
		ChatID:    chatID,
		State:     StateGathering,
		Messages:  []Message{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	m.threads[chatID] = conv
	return conv
}

// Get returns the active thread for a chat, if any.
func (m *Manager) Get(chatID string) (*Thread, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conv, ok := m.threads[chatID]
	if !ok || conv.GetState() == StateCompleted {
		return nil, false
	}
	return conv, true
}

// Complete marks the thread for a chat as completed and removes its
// persisted file (if any).
func (m *Manager) Complete(chatID string) {
	m.mu.RLock()
	conv, ok := m.threads[chatID]
	m.mu.RUnlock()
	if ok {
		conv.SetState(StateCompleted)
		if m.dir != "" {
			os.Remove(m.convFilePath(chatID))
		}
	}
}
