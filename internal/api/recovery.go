package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/sessionjournal"
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
	for _, entry := range entries {
		switch entry.Status {
		case "queued":
			s.recoverQueued(ctx, entry, hooks)
		case "running":
			s.recoverInterrupted(ctx, entry, hooks)
		default:
			slog.Warn("session recovery: unknown journal status, dropping", "session_id", entry.SessionID, "status", entry.Status)
			s.journal.Remove(entry.SessionID)
		}
	}
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
	s.notifyRecovery(ctx, entry, hooks, fmt.Sprintf(
		"⚠️ The task %q was interrupted by a server restart. Resend it to retry.",
		textutil.Truncate(entry.Message, 80)))
}

// recoverQueued re-enqueues a session that never started executing.
func (s *Server) recoverQueued(ctx context.Context, entry sessionjournal.Entry, hooks map[string]sourceHooks) {
	session := restoredSession(entry)
	session.Status = agent.SessionStatusQueued
	session.StartedAt = time.Now()

	if err := s.agentManager.RestoreSession(session); err != nil {
		slog.Warn("session recovery: restore failed", "session_id", entry.SessionID, "error", err)
		s.journal.Remove(entry.SessionID)
		return
	}
	if err := s.agentManager.Enqueue(session, s.handlers.execEngine.ExecuteAgent); err != nil {
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
		StartedAt:           entry.CreatedAt,
	}
}
