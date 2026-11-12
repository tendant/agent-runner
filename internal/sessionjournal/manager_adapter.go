package sessionjournal

import (
	"log/slog"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// ManagerJournal adapts a Journal to agent.Manager's hook interface. Journal
// failures are logged, never propagated — persistence must not affect session
// execution.
type ManagerJournal struct {
	journal *Journal
}

// ForManager wraps j for use with agent.Manager.SetJournal.
func ForManager(j *Journal) *ManagerJournal {
	return &ManagerJournal{journal: j}
}

func (m *ManagerJournal) record(snap *agent.Session, status string) {
	entry := Entry{
		SessionID:       snap.ID,
		Source:          snap.Source,
		ConvID:          snap.ConvID,
		Message:         snap.Message,
		Paths:           snap.Paths,
		Author:          snap.Author,
		CommitPrefix:    snap.CommitMessagePrefix,
		MaxIterations:   snap.MaxIterations,
		MaxTotalSeconds: snap.MaxTotalSeconds,
		CallbackURL:     snap.CallbackURL,
		TaskWorkspace:   snap.TaskWorkspace,
		TaskFeedback:    snap.TaskFeedback,
		Status:          status,
		CreatedAt:       createdAt(snap),
	}
	if err := m.journal.Write(entry); err != nil {
		slog.Warn("session journal: write failed", "session_id", snap.ID, "error", err)
	}
}

func (m *ManagerJournal) RecordQueued(snap *agent.Session)  { m.record(snap, "queued") }
func (m *ManagerJournal) RecordRunning(snap *agent.Session) { m.record(snap, "running") }

func (m *ManagerJournal) Remove(sessionID string) {
	if err := m.journal.Remove(sessionID); err != nil {
		slog.Warn("session journal: remove failed", "session_id", sessionID, "error", err)
	}
}

// createdAt is when the session was queued; StartedAt, for sessions made
// before QueuedAt existed.
func createdAt(snap *agent.Session) time.Time {
	if !snap.QueuedAt.IsZero() {
		return snap.QueuedAt
	}
	return snap.StartedAt
}
