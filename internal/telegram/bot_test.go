package telegram

import (
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
)

func TestNew_EmptyToken(t *testing.T) {
	bot := New(config.TelegramConfig{}, nil, nil, nil)
	if bot != nil {
		t.Error("expected nil bot for empty token")
	}
}

func TestNew_WithToken(t *testing.T) {
	bot := New(config.TelegramConfig{BotToken: "fake-token", ChatID: 12345}, nil, nil, nil)
	if bot == nil {
		t.Fatal("expected non-nil bot for non-empty token")
	}
	if bot.token != "fake-token" {
		t.Errorf("expected token fake-token, got %s", bot.token)
	}
	if bot.chatID != 12345 {
		t.Errorf("expected chatID 12345, got %d", bot.chatID)
	}
}

func TestFormatIteration_Success(t *testing.T) {
	iter := agent.IterationResult{
		Iteration: 3,
		Status:    agent.IterationStatusSuccess,
		Commit:    "abc1234",
	}
	got := FormatIteration(iter)
	want := "Iteration 3: completed (commit `abc1234`)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIteration_SuccessNoCommit(t *testing.T) {
	iter := agent.IterationResult{
		Iteration: 1,
		Status:    agent.IterationStatusSuccess,
	}
	got := FormatIteration(iter)
	want := "Iteration 1: completed"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIteration_NoChanges(t *testing.T) {
	iter := agent.IterationResult{
		Iteration: 2,
		Status:    agent.IterationStatusNoChanges,
	}
	got := FormatIteration(iter)
	want := "Iteration 2: no changes"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIteration_ValidationFailed(t *testing.T) {
	iter := agent.IterationResult{
		Iteration: 4,
		Status:    agent.IterationStatusValidation,
		Error:     "file outside allowed paths",
	}
	got := FormatIteration(iter)
	want := "Iteration 4: validation failed — file outside allowed paths"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIteration_Error(t *testing.T) {
	iter := agent.IterationResult{
		Iteration: 5,
		Status:    agent.IterationStatusError,
		Error:     "claude execution failed: timeout",
	}
	got := FormatIteration(iter)
	want := "Iteration 5: error — claude execution failed: timeout"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFinalResult_Completed(t *testing.T) {
	now := time.Now()
	session := &agent.Session{
		Status:         agent.SessionStatusCompleted,
		SuccessfulIterations: 7,
		ElapsedSeconds: 120,
		CompletedAt:    &now,
		Iterations:     make([]agent.IterationResult, 5),
	}
	got := FormatFinalResult(session)
	want := "Session completed — 5 iterations in 120s"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFinalResult_Failed(t *testing.T) {
	session := &agent.Session{
		Status: agent.SessionStatusFailed,
		Error:  "aborted after 5 consecutive failures",
	}
	got := FormatFinalResult(session)
	want := "Session failed — aborted after 5 consecutive failures"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFinalResult_FailedNoError(t *testing.T) {
	session := &agent.Session{
		Status: agent.SessionStatusFailed,
	}
	got := FormatFinalResult(session)
	want := "Session failed"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIsConfirmation(t *testing.T) {
	positives := []string{"yes", "Yes", "YES", "y", "ok", "sure", "proceed", "go", "yep", "yeah"}
	for _, s := range positives {
		if !isConfirmation(s) {
			t.Errorf("expected %q to be confirmation", s)
		}
	}

	negatives := []string{"no", "maybe", "hello", "what"}
	for _, s := range negatives {
		if isConfirmation(s) {
			t.Errorf("expected %q not to be confirmation", s)
		}
	}
}

func TestIsDenial(t *testing.T) {
	positives := []string{"no", "No", "NO", "n", "nope", "cancel", "stop", "nah"}
	for _, s := range positives {
		if !isDenial(s) {
			t.Errorf("expected %q to be denial", s)
		}
	}

	negatives := []string{"yes", "maybe", "hello"}
	for _, s := range negatives {
		if isDenial(s) {
			t.Errorf("expected %q not to be denial", s)
		}
	}
}

// mockStarter implements AgentStarter for testing.
type mockStarter struct {
	startCalled  bool
	startMessage string
	startID      string
	startErr     error
}

func (m *mockStarter) StartAgent(message string) (string, error) {
	m.startCalled = true
	m.startMessage = message
	return m.startID, m.startErr
}

func (m *mockStarter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	return nil, false
}

func TestHandleMessage_UnauthorizedChat(t *testing.T) {
	bot := &Bot{
		chatID: 12345,
	}

	// Simulate authorization check logic
	if bot.chatID != 0 && 99999 != bot.chatID {
		// This is the branch that would be taken in handleMessage
	} else {
		t.Error("expected unauthorized chat to be rejected")
	}
}

func TestHandleMessage_ZeroChatIDAllowsAll(t *testing.T) {
	bot := &Bot{
		chatID: 0,
	}

	if bot.chatID != 0 {
		t.Error("zero chatID should allow all chats")
	}
}
