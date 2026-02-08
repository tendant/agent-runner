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

// executeAgent runs the agent iteration loop
func (h *Handlers) executeAgent(session *agent.Session, projectPath string) {
	sessionID := session.ID
	project := session.Project
	promptFile := session.PromptFile
	paths := session.Paths
	authorName := session.Author
	commitPrefix := session.CommitMessagePrefix
	maxIter := session.MaxIterations
	maxSeconds := session.MaxTotalSeconds

	startTime := time.Now()
	deadline := startTime.Add(time.Duration(maxSeconds) * time.Second)

	// Get the live session reference for mutations
	liveSession, _ := h.agentManager.GetSessionDirect(sessionID)

	defer func() {
		// Write agent audit log
		snap := liveSession.Snapshot()
		logData := &logging.AgentLogData{
			SessionID:    sessionID,
			Project:      project,
			Status:       string(snap.Status),
			Duration:     int(time.Since(startTime).Seconds()),
			PromptFile:   promptFile,
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

	// Step 1: Fetch and reset the source project
	ctx := context.Background()
	if err := h.gitOps.FetchAndReset(ctx, projectPath); err != nil {
		h.agentManager.FailSession(sessionID, "Failed to prepare git repository: "+err.Error())
		return
	}

	// Step 2: Prepare persistent workspace
	workspacePath, err := h.workspaceManager.PrepareWorkspace(projectPath, sessionID)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to prepare workspace: "+err.Error())
		return
	}
	defer h.workspaceManager.CleanupWorkspace(workspacePath)

	// Configure git author in workspace
	if err := h.gitOps.ConfigureAuthor(ctx, workspacePath, authorName); err != nil {
		h.agentManager.FailSession(sessionID, "Failed to configure git author: "+err.Error())
		return
	}

	// Iteration loop
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

		result := h.executeIteration(ctx, liveSession, workspacePath, promptFile, paths, commitPrefix, authorName, i, deadline)
		liveSession.AddIteration(result)
	}

	h.agentManager.CompleteSession(sessionID)
}

// executeIteration runs a single iteration of the agent loop
func (h *Handlers) executeIteration(
	ctx context.Context,
	session *agent.Session,
	workspacePath, promptFile string,
	paths []string,
	commitPrefix, author string,
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

	// Step 1: Read prompt file from workspace
	promptPath := filepath.Join(workspacePath, promptFile)
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("failed to read prompt file %s: %v", promptFile, err)
		return result
	}
	prompt := strings.TrimSpace(string(promptData))
	if prompt == "" {
		result.Status = agent.IterationStatusError
		result.Error = "prompt file is empty"
		return result
	}

	// Step 2: Execute Claude Code
	_, _, execErr := h.executor.ExecuteWithLog(iterCtx, workspacePath, prompt)
	if execErr != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("claude execution failed: %v", execErr)
		// Revert any partial changes
		h.gitOps.RevertChanges(ctx, workspacePath)
		return result
	}

	// Step 3: Check for changes
	changedFiles, err := h.gitOps.GetChangedFiles(iterCtx, workspacePath)
	if err != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("failed to get changed files: %v", err)
		return result
	}

	if len(changedFiles) == 0 {
		result.Status = agent.IterationStatusNoChanges
		return result
	}

	result.ChangedFiles = changedFiles

	// Step 4: Validate diff
	validationErr := h.validator.ValidateDiff(changedFiles, paths)
	if validationErr != nil {
		result.Status = agent.IterationStatusValidation
		result.Error = validationErr.Message
		// Revert invalid changes
		h.gitOps.RevertChanges(ctx, workspacePath)
		return result
	}

	// Step 5: Commit
	commitMsg := fmt.Sprintf("%s iteration %d", commitPrefix, iteration)
	commitHash, err := h.gitOps.Commit(iterCtx, workspacePath, commitMsg, author, "")
	if err != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("commit failed: %v", err)
		h.gitOps.RevertChanges(ctx, workspacePath)
		return result
	}
	result.Commit = commitHash

	// Step 6: Push (with pull --rebase on conflict)
	if err := h.gitOps.Push(iterCtx, workspacePath); err != nil {
		if strings.Contains(err.Error(), "GIT_PUSH_CONFLICT") {
			// Try pull --rebase then push again
			if rebaseErr := h.gitOps.PullRebase(iterCtx, workspacePath); rebaseErr != nil {
				result.Status = agent.IterationStatusError
				result.Error = fmt.Sprintf("push conflict, rebase failed: %v", rebaseErr)
				return result
			}
			if retryErr := h.gitOps.Push(iterCtx, workspacePath); retryErr != nil {
				result.Status = agent.IterationStatusError
				result.Error = fmt.Sprintf("push failed after rebase: %v", retryErr)
				return result
			}
		} else {
			result.Status = agent.IterationStatusError
			result.Error = fmt.Sprintf("push failed: %v", err)
			return result
		}
	}

	result.Status = agent.IterationStatusSuccess
	return result
}
