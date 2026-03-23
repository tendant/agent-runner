package conversation

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

// State represents the current phase of a conversation.
type State string

const (
	StateGathering  State = "gathering"  // collecting information
	StateConfirming State = "confirming" // plan shown, awaiting yes/no
	StateExecuting  State = "executing"  // agent running
	StateCompleted  State = "completed"
)

// Message is a single message in the conversation history.
type Message struct {
	Role    string    `json:"role"` // "user" or "assistant"
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
}

// Conversation tracks an ongoing chat with a user.
type Conversation struct {
	mu sync.Mutex

	ID           string
	ChatID       string
	State        State
	Messages     []Message
	Plan         string // generated plan text
	pendingInput bool   // true if user sent messages during execution
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const maxMessages = 20

// Summarizer can condense conversation history into a short summary.
type Summarizer interface {
	Summarize(ctx context.Context, messages []Message) (string, error)
}

// AddMessage appends a message to the conversation and updates the timestamp.
// When messages exceed maxMessages, old messages are compacted: if a Summarizer
// is set, they are summarized; otherwise the oldest are dropped.
func (c *Conversation) AddMessage(role, content string) {
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
func (c *Conversation) compact() {
	if len(c.Messages) <= maxMessages {
		return
	}
	// Keep the last maxMessages messages, preserving any leading summary
	c.Messages = c.Messages[len(c.Messages)-maxMessages:]
}

// NeedsCompaction returns true if the conversation has enough messages to
// benefit from summarization (called before triggering async summarization).
func (c *Conversation) NeedsCompaction() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Messages) >= maxMessages-2 // trigger before we hit the hard cap
}

// CompactWithSummary replaces old messages with a summary message, keeping
// the most recent keepRecent messages intact. Thread-safe.
func (c *Conversation) CompactWithSummary(summary string, keepRecent int) {
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

// SetState changes the conversation state.
func (c *Conversation) SetState(state State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.State = state
	c.UpdatedAt = time.Now()
}

// GetState returns the current state.
func (c *Conversation) GetState() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.State
}

// GetMessages returns a copy of the message history.
func (c *Conversation) GetMessages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	msgs := make([]Message, len(c.Messages))
	copy(msgs, c.Messages)
	return msgs
}

// SetPlan stores the generated plan text and transitions to confirming state.
func (c *Conversation) SetPlan(plan string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Plan = plan
	c.State = StateConfirming
	c.UpdatedAt = time.Now()
}

// GetPlan returns the stored plan text.
func (c *Conversation) GetPlan() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Plan
}

// GetUserMessage returns the concatenation of all user messages in the conversation.
func (c *Conversation) GetUserMessage() string {
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

// Reset transitions a completed conversation back to gathering state,
// preserving message history while clearing the plan.
func (c *Conversation) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Plan = ""
	c.pendingInput = false
	c.State = StateGathering
	c.UpdatedAt = time.Now()
}

// ClearPendingInput atomically checks and clears the pending input flag.
// Returns true if user messages were queued during execution.
func (c *Conversation) ClearPendingInput() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.pendingInput
	c.pendingInput = false
	return had
}

// GetFormattedHistory returns the full conversation history formatted for
// inclusion in a prompt. Returns empty string if there are fewer than 2 messages
// (i.e., only the current message exists, so no prior context).
func (c *Conversation) GetFormattedHistory() string {
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

const conversationIdleTimeout = 30 * time.Minute

// Manager manages active conversations keyed by chat/channel ID.
type Manager struct {
	mu            sync.RWMutex
	conversations map[string]*Conversation
	nextID        int
	stopCh        chan struct{}
	dir           string // persistence directory; "" = disabled
}

// NewManager creates a new conversation manager. dir is the directory used to
// persist conversations to disk across restarts; pass "" to disable persistence.
func NewManager(dir string) *Manager {
	m := &Manager{
		conversations: make(map[string]*Conversation),
		stopCh:        make(chan struct{}),
		dir:           dir,
	}
	if dir != "" {
		m.loadAll()
	}
	go m.cleanupLoop()
	return m
}

// convFilePath returns the JSON file path for a conversation.
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

// persist writes a conversation to disk. No-op if persistence is disabled.
func (m *Manager) persist(conv *Conversation) {
	if m.dir == "" {
		return
	}
	conv.mu.Lock()
	data, err := json.Marshal(conv)
	conv.mu.Unlock()
	if err != nil {
		slog.Warn("conversation: marshal failed", "chat_id", conv.ChatID, "error", err)
		return
	}
	if err := os.WriteFile(m.convFilePath(conv.ChatID), data, 0600); err != nil {
		slog.Warn("conversation: persist failed", "chat_id", conv.ChatID, "error", err)
	}
}

// loadAll reads all persisted conversations from disk at startup.
func (m *Manager) loadAll() {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		slog.Warn("conversation: failed to create persistence dir", "dir", m.dir, "error", err)
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		slog.Warn("conversation: failed to read persistence dir", "dir", m.dir, "error", err)
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
			slog.Warn("conversation: failed to read file", "path", path, "error", err)
			continue
		}
		var conv Conversation
		if err := json.Unmarshal(data, &conv); err != nil {
			slog.Warn("conversation: failed to parse file", "path", path, "error", err)
			continue
		}
		if conv.State == StateCompleted {
			continue // skip completed conversations
		}
		if conv.ChatID == "" {
			continue
		}
		m.conversations[conv.ChatID] = &conv
		loaded++
	}
	if loaded > 0 {
		slog.Info("conversation: loaded persisted conversations", "count", loaded)
	}
}

// saveAll persists all active conversations. Called from the background loop.
func (m *Manager) saveAll() {
	if m.dir == "" {
		return
	}
	m.mu.RLock()
	convs := make([]*Conversation, 0, len(m.conversations))
	for _, conv := range m.conversations {
		convs = append(convs, conv)
	}
	m.mu.RUnlock()
	for _, conv := range convs {
		m.persist(conv)
	}
}

// Stop stops the cleanup loop. Safe to call multiple times.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// cleanupLoop periodically evicts stale conversations and saves active ones.
func (m *Manager) cleanupLoop() {
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

	cutoff := time.Now().Add(-conversationIdleTimeout)
	for chatID, conv := range m.conversations {
		conv.mu.Lock()
		updatedAt := conv.UpdatedAt
		conv.mu.Unlock()

		if updatedAt.Before(cutoff) {
			delete(m.conversations, chatID)
			if m.dir != "" {
				os.Remove(m.convFilePath(chatID))
			}
		}
	}
}

// GetOrCreate returns the active conversation for a chat, creating one if none exists.
// If the previous conversation is completed, it resets it to gathering state while
// preserving message history so the agent has context from prior sessions.
func (m *Manager) GetOrCreate(chatID string) *Conversation {
	m.mu.Lock()
	defer m.mu.Unlock()

	if conv, ok := m.conversations[chatID]; ok {
		if conv.GetState() == StateCompleted {
			conv.Reset()
		}
		return conv
	}

	m.nextID++
	conv := &Conversation{
		ID:        fmt.Sprintf("conv-%d", m.nextID),
		ChatID:    chatID,
		State:     StateGathering,
		Messages:  []Message{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	m.conversations[chatID] = conv
	return conv
}

// Get returns the active conversation for a chat, if any.
func (m *Manager) Get(chatID string) (*Conversation, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conv, ok := m.conversations[chatID]
	if !ok || conv.GetState() == StateCompleted {
		return nil, false
	}
	return conv, true
}

// Complete marks the conversation for a chat as completed and removes its
// persisted file (if any).
func (m *Manager) Complete(chatID string) {
	m.mu.RLock()
	conv, ok := m.conversations[chatID]
	m.mu.RUnlock()
	if ok {
		conv.SetState(StateCompleted)
		if m.dir != "" {
			os.Remove(m.convFilePath(chatID))
		}
	}
}
