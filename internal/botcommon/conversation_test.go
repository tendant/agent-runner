package botcommon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/conversation"
)

// fakeLLMClient implements llm.Client, returning a scripted response or error.
type fakeLLMClient struct {
	response string
	err      error
}

func (f *fakeLLMClient) Complete(ctx context.Context, prompt string) (string, error) {
	return f.response, f.err
}

func newTestConversation(n int) *conversation.Conversation {
	conv := &conversation.Conversation{}
	for i := 0; i < n; i++ {
		conv.AddMessage("user", "message")
	}
	return conv
}

func TestSummarizeConversation_CompactsOlderHalf(t *testing.T) {
	// 20 messages: keepRecent = max(20/2, 4) = 10, so the first 10 are summarized.
	conv := newTestConversation(20)
	analyzer := conversation.NewAnalyzer(&fakeLLMClient{response: "summary text"})

	SummarizeConversation(analyzer, conv, "test")

	msgs := conv.GetMessages()
	// CompactWithSummary prepends one summary message ahead of the kept-recent messages.
	if len(msgs) != 11 {
		t.Fatalf("expected 11 messages (1 summary + 10 recent), got %d", len(msgs))
	}
	if msgs[0].Content != "[Conversation summary]\nsummary text" {
		t.Errorf("expected summary message first, got: %q", msgs[0].Content)
	}
}

func TestSummarizeConversation_MinimumKeepRecent(t *testing.T) {
	// Fewer than 8 messages: keepRecent floors at 4 (len/2 would be < 4).
	conv := newTestConversation(6)
	analyzer := conversation.NewAnalyzer(&fakeLLMClient{response: "summary"})

	SummarizeConversation(analyzer, conv, "test")

	msgs := conv.GetMessages()
	if len(msgs) != 5 { // 1 summary + 4 kept recent
		t.Fatalf("expected 5 messages (1 summary + 4 recent), got %d", len(msgs))
	}
}

func TestSummarizeConversation_NothingToSummarize(t *testing.T) {
	// keepRecent floors at 4; with exactly 4 messages there's nothing older to summarize.
	conv := newTestConversation(4)
	analyzer := conversation.NewAnalyzer(&fakeLLMClient{response: "should not be used"})

	SummarizeConversation(analyzer, conv, "test")

	msgs := conv.GetMessages()
	if len(msgs) != 4 {
		t.Fatalf("expected conversation to be untouched (4 messages), got %d", len(msgs))
	}
}

func TestSummarizeConversation_AnalyzerErrorLeavesConversationUnchanged(t *testing.T) {
	conv := newTestConversation(20)
	analyzer := conversation.NewAnalyzer(&fakeLLMClient{err: errors.New("llm unavailable")})

	SummarizeConversation(analyzer, conv, "test")

	msgs := conv.GetMessages()
	if len(msgs) != 20 {
		t.Fatalf("expected conversation untouched on analyzer error, got %d messages", len(msgs))
	}
}

func TestSummarizeConversation_ReturnsWithinTimeout(t *testing.T) {
	// Sanity check that SummarizeConversation doesn't hang past its own
	// internal timeout even if the underlying client blocks past it.
	orig := summarizeTimeout
	summarizeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { summarizeTimeout = orig })

	conv := newTestConversation(20)
	analyzer := conversation.NewAnalyzer(&blockingLLMClient{})

	done := make(chan struct{})
	go func() {
		SummarizeConversation(analyzer, conv, "test")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SummarizeConversation did not return within its own timeout + margin")
	}
}

// blockingLLMClient blocks until its context is cancelled, simulating a slow
// or hung LLM call.
type blockingLLMClient struct{}

func (blockingLLMClient) Complete(ctx context.Context, prompt string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
