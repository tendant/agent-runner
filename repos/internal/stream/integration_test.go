//go:build integration

package stream

import (
	"context"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

const testToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpYXQiOjE3NzE0NzM5MjQsInN1YiI6InVfYzVmMDY5NGI3ZmE5YTc1OTdkN2M3MWI5In0.c7ACxqCGqttbGXk_0Ve2q-MW5EhlIzjzLoGvepLHTrQ"
const testServerURL = "https://bot.memochat.ai"
const testConvID = "c_8ef144979c0f1f499b8d83ca"

// mockStarter is a simple mock for integration testing.
type mockStarter struct{}

func (m *mockStarter) StartAgent(message string) (string, error) {
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
	cfg := config.StreamConfig{
		ServerURL:       testServerURL,
		BotToken:        testToken,
		ConversationIDs: []string{testConvID},
	}

	convMgr := conversation.NewManager()
	defer convMgr.Stop()

	bot := New(cfg, &mockStarter{}, convMgr, nil)
	if bot == nil {
		t.Fatal("expected non-nil bot")
	}

	t.Logf("Bot user ID extracted: %q", bot.botUserID)
	if bot.botUserID != "u_c5f0694b7fa9a7597d7c71b9" {
		t.Errorf("unexpected bot user ID: %s", bot.botUserID)
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
	client := NewClient(testServerURL, testToken)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test emitting each event type
	if err := client.EmitEvent(ctx, testConvID, "status.thinking", []byte(`{"message":"Integration test thinking..."}`)); err != nil {
		t.Fatalf("emit thinking failed: %v", err)
	}
	t.Log("Emitted status.thinking")

	if err := client.EmitEvent(ctx, testConvID, "assistant.delta", []byte(`{"delta":"Hello from "}`)); err != nil {
		t.Fatalf("emit delta failed: %v", err)
	}
	t.Log("Emitted assistant.delta")

	if err := client.EmitEvent(ctx, testConvID, "assistant.final", []byte(`{"content":"Hello from agent-runner integration test!"}`)); err != nil {
		t.Fatalf("emit final failed: %v", err)
	}
	t.Log("Emitted assistant.final")
}

func TestIntegration_SSEStream(t *testing.T) {
	client := NewClient(testServerURL, testToken)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.StreamEvents(ctx, testConvID, 0)
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
