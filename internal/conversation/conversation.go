package conversation

import (
	"fmt"
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

	ID        string
	ChatID    int64
	State     State
	Messages  []Message
	Plan      string // generated plan text
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AddMessage appends a message to the conversation and updates the timestamp.
func (c *Conversation) AddMessage(role, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Messages = append(c.Messages, Message{
		Role:    role,
		Content: content,
		Time:    time.Now(),
	})
	c.UpdatedAt = time.Now()
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

// Manager manages active conversations keyed by Telegram chat ID.
type Manager struct {
	mu            sync.RWMutex
	conversations map[int64]*Conversation
	nextID        int
}

// NewManager creates a new conversation manager.
func NewManager() *Manager {
	return &Manager{
		conversations: make(map[int64]*Conversation),
	}
}

// GetOrCreate returns the active conversation for a chat, creating one if none exists
// or the previous one is completed.
func (m *Manager) GetOrCreate(chatID int64) *Conversation {
	m.mu.Lock()
	defer m.mu.Unlock()

	if conv, ok := m.conversations[chatID]; ok {
		if conv.GetState() != StateCompleted {
			return conv
		}
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
func (m *Manager) Get(chatID int64) (*Conversation, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conv, ok := m.conversations[chatID]
	if !ok || conv.GetState() == StateCompleted {
		return nil, false
	}
	return conv, true
}

// Complete marks the conversation for a chat as completed.
func (m *Manager) Complete(chatID int64) {
	m.mu.RLock()
	conv, ok := m.conversations[chatID]
	m.mu.RUnlock()
	if ok {
		conv.SetState(StateCompleted)
	}
}
