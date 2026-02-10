package api

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/logging"
)

const maxConsecutiveFailures = 5

// executeAgent runs the agent iteration loop.
// Claude handles all git operations via the prompt — the loop just runs Claude
// in the workspace and records results.
func (h *Handlers) executeAgent(session *agent.Session) {
	sessionID := session.ID
	project := session.Project
	message := session.Message
	maxIter := session.MaxIterations
	maxSeconds := session.MaxTotalSeconds
	authorName := session.Author

	startTime := time.Now()
	deadline := startTime.Add(time.Duration(maxSeconds) * time.Second)

	// Get the live session reference for mutations
	liveSession, _ := h.agentManager.GetSessionDirect(sessionID)

	defer func() {
		// Cache repos back to projects for future runs
		if liveSession.WorkspacePath != "" {
			h.workspaceManager.CacheReposBack(liveSession.WorkspacePath, h.config.ProjectsRoot)
		}

		// Write agent audit log
		snap := liveSession.Snapshot()
		logData := &logging.AgentLogData{
			SessionID:    sessionID,
			Project:      project,
			Status:       string(snap.Status),
			Duration:     int(time.Since(startTime).Seconds()),
			Message:      message,
			Author:       authorName,
			TotalCommits: snap.TotalCommits,
			Error:        snap.Error,
		}
		for _, iter := range snap.Iterations {
			logData.Iterations = append(logData.Iterations, logging.AgentIterationLog{
				Iteration:    iter.Iteration,
				Status:       string(iter.Status),
				Commit:       iter.Commit,
				ChangedFiles: iter.ChangedFiles,
				Error:        iter.Error,
				DurationSecs: iter.DurationSecs,
			})
		}
		if logFile, err := h.runLogger.WriteAgentLog(logData); err != nil {
			log.Printf("Failed to write agent log: %v", err)
		} else {
			log.Printf("Agent log written: %s", logFile)
		}
	}()

	// Resolve prompt: read template file and inject message, or use message directly
	prompt, err := h.resolvePrompt(message)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to resolve prompt: "+err.Error())
		return
	}

	// Prepare agent workspace with repos/ structure
	workspacePath, err := h.workspaceManager.PrepareAgentWorkspace(
		h.config.ProjectsRoot, sessionID, project, h.config.Agent.SharedRepos,
	)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to prepare workspace: "+err.Error())
		return
	}
	liveSession.SetWorkspacePath(workspacePath)
	defer h.workspaceManager.CleanupWorkspace(workspacePath)

	// Claude runs in the repos/ subdirectory
	reposPath := filepath.Join(workspacePath, "repos")

	// Iteration loop
	ctx := context.Background()
	for i := 1; i <= maxIter; i++ {
		// Check stop signal
		if liveSession.StopRequested() {
			log.Printf("Agent %s: stop requested, exiting after iteration %d", sessionID, i-1)
			break
		}

		// Check time limit
		if time.Now().After(deadline) {
			log.Printf("Agent %s: time limit reached (%ds)", sessionID, maxSeconds)
			break
		}

		// Check consecutive failures
		consecutiveFails := liveSession.GetConsecutiveFailures()
		if consecutiveFails >= maxConsecutiveFailures {
			log.Printf("Agent %s: %d consecutive failures, aborting", sessionID, consecutiveFails)
			h.agentManager.FailSession(sessionID, fmt.Sprintf("aborted after %d consecutive failures", consecutiveFails))
			return
		}

		result := h.executeIteration(ctx, reposPath, prompt, i, deadline)
		liveSession.AddIteration(result)
	}

	h.agentManager.CompleteSession(sessionID)
}

// resolvePrompt reads the prompt template file and injects the message,
// or returns the message directly if no template is configured.
func (h *Handlers) resolvePrompt(message string) (string, error) {
	templatePath := h.config.Agent.PromptFile
	if templatePath == "" {
		return message, nil
	}

	data, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt template %s: %w", templatePath, err)
	}

	template := strings.TrimSpace(string(data))
	if template == "" {
		return "", fmt.Errorf("prompt template file is empty")
	}

	return strings.ReplaceAll(template, "{{MESSAGE}}", message), nil
}

// executeIteration runs a single iteration of the agent loop.
// It just runs Claude and records success or error — no git operations.
func (h *Handlers) executeIteration(
	ctx context.Context,
	workspacePath, prompt string,
	iteration int,
	deadline time.Time,
) agent.IterationResult {
	iterStart := time.Now()

	result := agent.IterationResult{
		Iteration: iteration,
	}

	defer func() {
		result.DurationSecs = int(time.Since(iterStart).Seconds())
	}()

	// Calculate per-iteration timeout: min of configured max and remaining time
	remaining := time.Until(deadline)
	iterTimeout := time.Duration(h.config.Agent.MaxIterationSeconds) * time.Second
	if remaining < iterTimeout {
		iterTimeout = remaining
	}
	if iterTimeout <= 0 {
		result.Status = agent.IterationStatusError
		result.Error = "time limit reached"
		return result
	}

	iterCtx, cancel := context.WithTimeout(ctx, iterTimeout)
	defer cancel()

	// Execute Claude Code with resolved prompt
	_, _, execErr := h.executor.ExecuteWithLog(iterCtx, workspacePath, prompt)
	if execErr != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("claude execution failed: %v", execErr)
		return result
	}

	result.Status = agent.IterationStatusSuccess
	return result
}
