package api

import (
	"context"
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
	msgPreview := payload.Message
	if len(msgPreview) > 80 {
		msgPreview = msgPreview[:80] + "..."
	}
	log.Printf("runner bridge: executing agent task session=%s message=%q max_iter=%d max_seconds=%d",
		sessionID, msgPreview, maxIter, maxSeconds)

	// Run executeAgent synchronously — the runner's lease extension keeps
	// the workflow_run lease alive while this blocks.
	startTime := time.Now()
	h.executeAgent(session)
	elapsed := time.Since(startTime)

	// Check final status (notification is handled by executeAgent's defer)
	snap, _ := h.agentManager.GetSession(sessionID)
	if snap != nil {
		log.Printf("runner bridge: session=%s completed status=%s iterations=%d elapsed=%s",
			sessionID, snap.Status, snap.SuccessfulIterations, elapsed.Round(time.Second))
	}

	if snap != nil && snap.Status == agent.SessionStatusFailed && snap.Error != "" {
		log.Printf("runner bridge: session=%s failed: %s", sessionID, snap.Error)
		return &agentTaskError{msg: snap.Error}
	}

	return nil
}

type agentTaskError struct {
	msg string
}

func (e *agentTaskError) Error() string {
	return e.msg
}
