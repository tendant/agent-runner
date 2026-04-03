//go:build integration

package stream

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

func integrationEnv(t *testing.T) (serverURL, token, convID string) {
	t.Helper()
	serverURL = os.Getenv("STREAM_SERVER_URL")
	token = os.Getenv("STREAM_BOT_TOKEN")
	convID = os.Getenv("STREAM_CONVERSATION_IDS")
	if serverURL == "" || token == "" || convID == "" {
		t.Skip("STREAM_SERVER_URL, STREAM_BOT_TOKEN, and STREAM_CONVERSATION_IDS must be set for integration tests")
	}
	return
}

// mockStarter is a simple mock for integration testing.
type mockStarter struct{}

func (m *mockStarter) StartAgent(message, source string) (string, error) {
	return "test-session-123", nil
}

func (m *mockStarter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	return &agent.Session{
		ID:     sessionID,
		Status: agent.SessionStatusCompleted,
		Iterations: []agent.IterationResult{
			{Iteration: 1, Status: agent.IterationStatusSuccess, Output: "done"},
		},
	}, true
}

func TestIntegration_BotConnects(t *testing.T) {
	serverURL, token, convID := integrationEnv(t)

	cfg := config.StreamConfig{
		ServerURL:       serverURL,
		BotToken:        token,
		ConversationIDs: []string{convID},
	}

	convMgr := conversation.NewManager("")
	defer convMgr.Stop()

	bot := New(cfg, &mockStarter{}, convMgr, nil)
	if bot == nil {
		t.Fatal("expected non-nil bot")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := bot.Start(ctx); err != nil {
		t.Fatalf("bot start failed: %v", err)
	}

	time.Sleep(3 * time.Second)
	bot.Stop()
	t.Log("Bot stopped successfully")
}

func TestIntegration_EmitEvent(t *testing.T) {
	serverURL, token, convID := integrationEnv(t)

	client := NewClient(serverURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.EmitEvent(ctx, convID, "status.thinking", []byte(`{"message":"Integration test thinking..."}`)); err != nil {
		t.Fatalf("emit thinking failed: %v", err)
	}
	t.Log("Emitted status.thinking")

	if err := client.EmitEvent(ctx, convID, "assistant.delta", []byte(`{"delta":"Hello from "}`)); err != nil {
		t.Fatalf("emit delta failed: %v", err)
	}
	t.Log("Emitted assistant.delta")

	if err := client.EmitEvent(ctx, convID, "assistant.final", []byte(`{"content":"Hello from agent-runner integration test!"}`)); err != nil {
		t.Fatalf("emit final failed: %v", err)
	}
	t.Log("Emitted assistant.final")
}

func TestIntegration_SSEStream(t *testing.T) {
	serverURL, token, convID := integrationEnv(t)

	client := NewClient(serverURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.StreamEvents(ctx, convID, 0)
	if err != nil {
		t.Fatalf("stream events failed: %v", err)
	}

	count := 0
	for event := range events {
		count++
		t.Logf("Event seq=%d type=%s", event.Seq, event.Type)
	}
	t.Logf("Received %d events before timeout", count)
	if count == 0 {
		t.Error("expected at least 1 event")
	}
}
