package api

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/execution"
	"github.com/agent-runner/agent-runner/internal/sessionjournal"
	"github.com/agent-runner/agent-runner/internal/task"
	"github.com/agent-runner/agent-runner/internal/textutil"
)

// sourceHooks carries a chat channel's recovery capabilities: a targeted send
// to the originating conversation and a watcher re-attach for re-enqueued
// sessions.
type sourceHooks struct {
	notify func(ctx context.Context, convID, text string)
	resume func(convID, sessionID string)
}

// recoveryHooks maps session sources to the started bots' capabilities.
func (s *Server) recoveryHooks() map[string]sourceHooks {
	hooks := make(map[string]sourceHooks)
	if s.telegramBot != nil {
		hooks["telegram"] = sourceHooks{notify: s.telegramBot.NotifyConversation, resume: s.telegramBot.ResumeSession}
	}
	if s.streamBot != nil {
		hooks["stream"] = sourceHooks{notify: s.streamBot.NotifyConversation, resume: s.streamBot.ResumeSession}
	}
	if s.wechatBot != nil {
		hooks["wechat"] = sourceHooks{notify: s.wechatBot.NotifyConversation, resume: s.wechatBot.ResumeSession}
	}
	return hooks
}

// recoverSessions replays the session journal after a restart: sessions that
// were RUNNING when the previous process died are marked failed with a
// targeted notification; sessions still QUEUED (never executed) are safely
// re-enqueued with their watcher re-attached. Called from Start after the
// bots are up; all failures are non-fatal.
func (s *Server) recoverSessions() {
	if s.journal == nil {
		return
	}
	entries, err := s.journal.LoadAll()
	if err != nil {
		slog.Warn("session recovery: journal unreadable", "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	s.recoverWith(entries, s.recoveryHooks())
}

// recoverWith applies recovery decisions for the given journal entries.
// Split from recoverSessions so tests can inject entries and hooks.
func (s *Server) recoverWith(entries []sessionjournal.Entry, hooks map[string]sourceHooks) {
	ctx := context.Background()
	// Running sessions first: a run re-attached takes a worker ahead of the
	// queued sessions re-enqueued after it (Manager.Adopt).
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Status == "running" && entries[j].Status != "running" })
	for _, entry := range entries {
		switch entry.Status {
		case "queued":
			s.recoverQueued(ctx, entry, hooks)
		case "running":
			if !s.reattach(ctx, entry, hooks) {
				s.recoverInterrupted(ctx, entry, hooks)
			}
		default:
			slog.Warn("session recovery: unknown journal status, dropping", "session_id", entry.SessionID, "status", entry.Status)
			s.journal.Remove(entry.SessionID)
		}
	}
}

// reattach takes back a session whose run is on another backend and still
// going (JOBS_DESIGN.md §10): its runner keeps reporting and sends its
// result here. It reports whether it did; a run that is gone, or on a
// backend this server no longer uses, is for recoverInterrupted.
func (s *Server) reattach(ctx context.Context, entry sessionjournal.Entry, hooks map[string]sourceHooks) bool {
	eng := s.handlers.execEngine
	if entry.Backend == "" || entry.Backend == "local" || entry.Backend != eng.Backend().Name() {
		return false
	}
	h := execution.Handle{Backend: entry.Backend, Ref: entry.Ref, Session: entry.SessionID}
	alive, _, err := eng.Backend().Alive(ctx, h)
	if err != nil || !alive {
		slog.Info("session recovery: the run is gone", "session_id", entry.SessionID, "backend", entry.Backend, "ref", entry.Ref, "error", err)
		return false
	}
	session := restoredSession(entry)
	session.Status = agent.SessionStatusRunning
	if err := s.agentManager.RestoreSession(session); err != nil {
		slog.Warn("session recovery: restore failed", "session_id", entry.SessionID, "error", err)
		return false
	}
	eng.Reattach(session, h)
	slog.Info("session recovery: re-attached to a running session", "session_id", entry.SessionID, "backend", entry.Backend, "ref", entry.Ref)
	if hk, ok := hooks[entry.Source]; ok && hk.resume != nil && entry.ConvID != "" {
		hk.resume(entry.ConvID, entry.SessionID)
	}
	return true
}

// recoverInterrupted marks a session that was mid-run as failed and tells the
// originating conversation.
func (s *Server) recoverInterrupted(ctx context.Context, entry sessionjournal.Entry, hooks map[string]sourceHooks) {
	now := time.Now()
	session := restoredSession(entry)
	session.Status = agent.SessionStatusFailed
	session.Error = "interrupted by server restart"
	session.CompletedAt = &now
	session.ElapsedSeconds = int(now.Sub(entry.CreatedAt).Seconds())

	if err := s.agentManager.RestoreSession(session); err != nil {
		slog.Warn("session recovery: restore failed", "session_id", entry.SessionID, "error", err)
	}
	s.journal.Remove(entry.SessionID)

	slog.Info("session recovery: interrupted session failed", "session_id", entry.SessionID, "source", entry.Source)
	if session.CallbackURL != "" {
		s.handlers.callbacks.Deliver(session.ID, session.CallbackURL, execution.CallbackPayload(session, ""))
	}
	s.notifyRecovery(ctx, entry, hooks, fmt.Sprintf(
		"⚠️ The task %q was interrupted by a server restart. Resend it to retry.",
		textutil.Truncate(entry.Message, 80)))
}

// recoverQueued re-enqueues a session that never started executing.
func (s *Server) recoverQueued(ctx context.Context, entry sessionjournal.Entry, hooks map[string]sourceHooks) {
	if entry.Source == "eval" {
		// An eval replay's spec (its instruction overrides and checks) is not
		// journaled, and the comparison that wanted it is gone: never run it
		// again as an ordinary session, which would keep and push its work.
		s.journal.Remove(entry.SessionID)
		slog.Info("session recovery: queued eval replay dropped", "session_id", entry.SessionID)
		return
	}
	session := restoredSession(entry)
	session.Status = agent.SessionStatusQueued
	session.StartedAt = time.Now()

	if err := s.agentManager.RestoreSession(session); err != nil {
		slog.Warn("session recovery: restore failed", "session_id", entry.SessionID, "error", err)
		s.journal.Remove(entry.SessionID)
		return
	}
	if err := s.agentManager.Enqueue(session, s.handlers.execEngine.RunQueued); err != nil {
		s.agentManager.FailSession(session.ID, "interrupted by server restart (queue full on recovery)")
		s.notifyRecovery(ctx, entry, hooks, fmt.Sprintf(
			"⚠️ The queued task %q could not be recovered after a server restart. Resend it to retry.",
			textutil.Truncate(entry.Message, 80)))
		return
	}

	slog.Info("session recovery: queued session re-enqueued", "session_id", entry.SessionID, "source", entry.Source)
	s.notifyRecovery(ctx, entry, hooks, fmt.Sprintf(
		"The server restarted; your queued task %q was requeued and will run shortly.",
		textutil.Truncate(entry.Message, 80)))
	if h, ok := hooks[entry.Source]; ok && h.resume != nil && entry.ConvID != "" {
		h.resume(entry.ConvID, entry.SessionID)
	}
}

// notifyRecovery delivers a recovery notice: targeted to the originating
// conversation when possible, else broadcast to the notification channels.
func (s *Server) notifyRecovery(ctx context.Context, entry sessionjournal.Entry, hooks map[string]sourceHooks, text string) {
	if h, ok := hooks[entry.Source]; ok && h.notify != nil && entry.ConvID != "" {
		h.notify(ctx, entry.ConvID, text)
		return
	}
	if s.handlers.notifier != nil {
		if err := s.handlers.notifier.SendNotification(ctx, text); err != nil {
			slog.Warn("session recovery: broadcast notification failed", "error", err)
		}
	}
}

// restoredSession rebuilds the common session fields from a journal entry.
func restoredSession(entry sessionjournal.Entry) *agent.Session {
	return &agent.Session{
		ID:                  entry.SessionID,
		Message:             entry.Message,
		Paths:               entry.Paths,
		Author:              entry.Author,
		CommitMessagePrefix: entry.CommitPrefix,
		MaxIterations:       entry.MaxIterations,
		MaxTotalSeconds:     entry.MaxTotalSeconds,
		Source:              entry.Source,
		ConvID:              entry.ConvID,
		CallbackURL:         entry.CallbackURL,
		QueuedAt:            entry.CreatedAt,
		StartedAt:           entry.CreatedAt,
		TaskWorkspace:       entry.TaskWorkspace,
		TaskFeedback:        entry.TaskFeedback,
	}
}

// recoverTasks pauses task records whose running turn did not survive the
// restart (TASKS_DESIGN.md §11). A turn re-enqueued by recoverSessions is
// still known to the agent manager and its re-attached watcher updates the
// record; any other "working" task lost its turn, and its intact workspace
// lets the next message in the thread continue it.
func (s *Server) recoverTasks() {
	if s.tasks == nil {
		return
	}
	for _, rec := range s.tasks.List() {
		if rec.Status != task.StatusWorking {
			continue
		}
		if t := rec.LastTurn(); t != nil {
			// Live only if it is still to run: recoverSessions also
			// registers interrupted sessions, already failed.
			if sess, ok := s.agentManager.GetSession(t.SessionID); ok {
				if st := sess.Snapshot().Status; st == agent.SessionStatusQueued || st == agent.SessionStatusRunning {
					continue
				}
			}
		}
		_, _ = s.tasks.Update(rec.ThreadKey, func(r *task.Record) {
			if r.Status != task.StatusWorking {
				return
			}
			r.Status = task.StatusPaused
			if t := r.LastTurn(); t != nil && t.EndedAt.IsZero() {
				t.EndedAt = time.Now()
				t.StopReason = "interrupted by restart"
			}
		})
		slog.Info("task paused: its turn was interrupted by restart", "task_id", rec.ID, "thread", rec.ThreadKey)
	}
}
