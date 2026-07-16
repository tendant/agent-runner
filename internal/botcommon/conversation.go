package botcommon

import (
	"context"
	"log/slog"
	"time"

	"github.com/agent-runner/agent-runner/internal/conversation"
)

// minKeepRecent is the minimum number of recent messages SummarizeConversation
// always keeps intact, even for short conversations.
const minKeepRecent = 4

// summarizeTimeout bounds how long a single summarization call may take.
// A var, not a const, so tests can shrink it instead of waiting on a real
// 30-second timeout.
var summarizeTimeout = 30 * time.Second

// SummarizeConversation compacts the older half of conv's messages into a
// summary via analyzer.Summarize, keeping at least minKeepRecent recent
// messages verbatim. No-ops if there's nothing to summarize. label prefixes
// the resulting slog messages (e.g. "telegram", "stream bot", "wechat").
func SummarizeConversation(analyzer *conversation.Analyzer, conv *conversation.Conversation, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), summarizeTimeout)
	defer cancel()

	msgs := conv.GetMessages()
	keepRecent := len(msgs) / 2
	if keepRecent < minKeepRecent {
		keepRecent = minKeepRecent
	}
	toSummarize := msgs[:len(msgs)-keepRecent]
	if len(toSummarize) == 0 {
		return
	}

	summary, err := analyzer.Summarize(ctx, toSummarize)
	if err != nil {
		slog.Warn(label+": conversation summarization failed", "error", err)
		return
	}
	conv.CompactWithSummary(summary, keepRecent)
	slog.Info(label+": conversation compacted", "summary_len", len(summary), "kept_recent", keepRecent)
}
