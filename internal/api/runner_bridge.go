package api

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/runner"
)

// RunnerBridge adapts the runner.AgentExecutor interface to the existing
// Handlers.executeAgent() pipeline. It bypasses the in-memory dispatch queue
// since the runner's lease-based claiming already serializes work.
type RunnerBridge struct {
	handlers *Handlers
}

// NewRunnerBridge creates a new RunnerBridge.
func NewRunnerBridge(handlers *Handlers) *RunnerBridge {
	return &RunnerBridge{handlers: handlers}
}

// ExecuteAgentTask implements runner.AgentExecutor.
// It creates a session via agentManager and runs executeAgent synchronously.
func (b *RunnerBridge) ExecuteAgentTask(ctx context.Context, payload runner.AgentTaskPayload) error {
	h := b.handlers

	// Use payload values or fall back to config defaults
	paths := payload.Paths
	if len(paths) == 0 {
		paths = h.config.Agent.Paths
	}
	author := payload.Author
	if author == "" {
		author = h.config.Agent.Author
	}
	maxIter := payload.MaxIterations
	if maxIter <= 0 {
		maxIter = h.config.Agent.MaxIterations
	}
	maxSeconds := payload.MaxTotalSeconds
	if maxSeconds <= 0 {
		maxSeconds = h.config.Agent.MaxTotalSeconds
	}

	// Create session in the agent manager (for status tracking)
	session, err := h.agentManager.CreateSession(
		payload.Message, paths,
		author, h.config.Agent.CommitPrefix,
		maxIter, maxSeconds,
	)
	if err != nil {
		return err
	}

	sessionID := session.ID
	log.Printf("runner bridge: executing agent task session=%s message_len=%d", sessionID, len(payload.Message))

	// Run executeAgent synchronously — the runner's lease extension keeps
	// the workflow_run lease alive while this blocks.
	h.executeAgent(session)

	// Check final status and notify
	snap, _ := h.agentManager.GetSession(sessionID)
	if snap != nil {
		b.notify(ctx, snap)
	}

	if snap != nil && snap.Status == agent.SessionStatusFailed && snap.Error != "" {
		return &agentTaskError{msg: snap.Error}
	}

	return nil
}

// notify sends a completion notification via the configured notifier (stream/telegram).
func (b *RunnerBridge) notify(ctx context.Context, snap *agent.Session) {
	if b.handlers.notifier == nil {
		return
	}

	preview := snap.Message
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}

	var msg string
	switch snap.Status {
	case agent.SessionStatusCompleted:
		msg = fmt.Sprintf("✅ Runner task completed\n• Session: %s\n• Message: %s\n• Iterations: %d\n• Duration: %ds",
			snap.ID, preview, snap.SuccessfulIterations, snap.ElapsedSeconds)
	case agent.SessionStatusFailed:
		errPreview := snap.Error
		if len(errPreview) > 120 {
			errPreview = errPreview[:120] + "..."
		}
		msg = fmt.Sprintf("❌ Runner task failed\n• Session: %s\n• Message: %s\n• Error: %s",
			snap.ID, preview, errPreview)
	default:
		return
	}

	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := b.handlers.notifier.SendNotification(notifyCtx, msg); err != nil {
		log.Printf("runner bridge: notification failed: %v", err)
	}
}

type agentTaskError struct {
	msg string
}

func (e *agentTaskError) Error() string {
	return e.msg
}
