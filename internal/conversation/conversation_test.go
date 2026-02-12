package conversation

import (
	"testing"
)

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestGetOrCreate_CreatesNew(t *testing.T) {
	m := NewManager()
	conv := m.GetOrCreate(12345)
	if conv == nil {
		t.Fatal("expected non-nil conversation")
	}
	if conv.ChatID != 12345 {
		t.Errorf("expected chatID 12345, got %d", conv.ChatID)
	}
	if conv.GetState() != StateGathering {
		t.Errorf("expected gathering state, got %s", conv.GetState())
	}
}

func TestGetOrCreate_ReturnsSame(t *testing.T) {
	m := NewManager()
	conv1 := m.GetOrCreate(12345)
	conv2 := m.GetOrCreate(12345)
	if conv1 != conv2 {
		t.Error("expected same conversation object")
	}
}

func TestGetOrCreate_NewAfterCompleted(t *testing.T) {
	m := NewManager()
	conv1 := m.GetOrCreate(12345)
	conv1.SetState(StateCompleted)

	conv2 := m.GetOrCreate(12345)
	if conv1 == conv2 {
		t.Error("expected new conversation after completion")
	}
	if conv2.GetState() != StateGathering {
		t.Errorf("expected gathering state, got %s", conv2.GetState())
	}
}

func TestGetOrCreate_DifferentChats(t *testing.T) {
	m := NewManager()
	conv1 := m.GetOrCreate(111)
	conv2 := m.GetOrCreate(222)
	if conv1 == conv2 {
		t.Error("expected different conversations for different chats")
	}
}

func TestGet_NotFound(t *testing.T) {
	m := NewManager()
	conv, ok := m.Get(99999)
	if ok || conv != nil {
		t.Error("expected nil for unknown chat")
	}
}

func TestGet_Completed(t *testing.T) {
	m := NewManager()
	conv := m.GetOrCreate(12345)
	conv.SetState(StateCompleted)

	got, ok := m.Get(12345)
	if ok || got != nil {
		t.Error("expected nil for completed conversation")
	}
}

func TestComplete(t *testing.T) {
	m := NewManager()
	conv := m.GetOrCreate(12345)
	m.Complete(12345)
	if conv.GetState() != StateCompleted {
		t.Errorf("expected completed, got %s", conv.GetState())
	}
}

func TestConversation_AddMessage(t *testing.T) {
	conv := &Conversation{
		State:    StateGathering,
		Messages: []Message{},
	}

	conv.AddMessage("user", "hello")
	conv.AddMessage("assistant", "hi there")

	msgs := conv.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "hi there" {
		t.Errorf("unexpected second message: %+v", msgs[1])
	}
}

func TestConversation_GetMessages_IsCopy(t *testing.T) {
	conv := &Conversation{
		State:    StateGathering,
		Messages: []Message{},
	}
	conv.AddMessage("user", "hello")

	msgs := conv.GetMessages()
	msgs[0].Content = "modified"

	original := conv.GetMessages()
	if original[0].Content != "hello" {
		t.Error("GetMessages should return a copy, not the original slice")
	}
}

func TestConversation_SetPlan(t *testing.T) {
	conv := &Conversation{
		State:    StateGathering,
		Messages: []Message{},
	}

	conv.SetPlan("Build a website with Hugo")
	if conv.GetPlan() != "Build a website with Hugo" {
		t.Errorf("unexpected plan: %s", conv.GetPlan())
	}
	if conv.GetState() != StateConfirming {
		t.Errorf("expected confirming state, got %s", conv.GetState())
	}
}

