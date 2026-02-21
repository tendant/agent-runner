package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

const maxConsecutiveFailures = 5

// executeAgent runs the agent iteration loop.
// Claude handles all git operations via the prompt — the loop just runs Claude
// in the workspace and records results.
func (h *Handlers) executeAgent(session *agent.Session) {
	sessionID := session.ID
	message := session.Message
	maxIter := session.MaxIterations
	maxSeconds := session.MaxTotalSeconds
	authorName := session.Author

	startTime := time.Now()
	deadline := startTime.Add(time.Duration(maxSeconds) * time.Second)

	// Get the live session reference for mutations
	liveSession, _ := h.agentManager.GetSessionDirect(sessionID)
	var plannerPromptText string

	defer func() {
		// Cache repos back for future runs
		if liveSession.WorkspacePath != "" {
			h.workspaceManager.CacheReposBack(liveSession.WorkspacePath, h.config.ReposRoot)
		}

		// Sync non-repo workspace files back to project dir
		if liveSession.WorkspacePath != "" {
			if err := h.workspaceManager.SyncBackToCWD(h.config.ProjectDir, liveSession.WorkspacePath, h.config.Agent.SharedRepos); err != nil {
				log.Printf("Agent %s: warning: failed to sync back to project dir: %v", sessionID, err)
			}
		}

		// Write agent audit log
		snap := liveSession.Snapshot()
		logData := &logging.AgentLogData{
			SessionID:    sessionID,
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
				Prompt:       iter.Prompt,
				Output:       iter.Output,
			})
		}
		logData.PlannerPrompt = plannerPromptText
		// Include plan/review in audit log
		if snap.PlanJSON != nil {
			if data, err := json.Marshal(snap.PlanJSON); err == nil {
				logData.Plan = string(data)
			}
		}
		if snap.ReviewJSON != nil {
			if data, err := json.Marshal(snap.ReviewJSON); err == nil {
				logData.Review = string(data)
			}
		}
		if logFile, err := h.runLogger.WriteAgentLog(logData); err != nil {
			log.Printf("Failed to write agent log: %v", err)
		} else {
			log.Printf("Agent log written: %s", logFile)
		}

		// Cleanup workspace after cache-back and logging are done
		if liveSession.WorkspacePath != "" {
			h.workspaceManager.CleanupWorkspace(liveSession.WorkspacePath)
		}
	}()

	// Resolve prompt: combine base system prompt (agent.md) + workflow template (prompt.md)
	if h.config.Agent.SystemPrompt != "" {
		log.Printf("Agent %s: system prompt: %s", sessionID, h.config.Agent.SystemPrompt)
	}
	if h.config.Agent.PromptFile != "" {
		log.Printf("Agent %s: workflow prompt: %s", sessionID, h.config.Agent.PromptFile)
	}
	preamble, err := h.resolvePrompt(message)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to resolve prompt: "+err.Error())
		return
	}

	// Prepare agent workspace with repos/ structure
	workspacePath, err := h.workspaceManager.PrepareAgentWorkspace(
		h.config.ReposRoot, sessionID, h.config.Agent.SharedRepos,
		h.config.GitHost, h.config.GitOrg,
	)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to prepare workspace: "+err.Error())
		return
	}
	liveSession.SetWorkspacePath(workspacePath)
	// NOTE: cleanup is done in the top-level defer (after CacheReposBack), not here.
	// A defer here would run BEFORE the earlier defer (LIFO), deleting the workspace
	// before cache-back can copy from it.

	// Copy project files into workspace so Claude can access them
	if err := h.workspaceManager.PopulateCWDFiles(h.config.ProjectDir, workspacePath); err != nil {
		log.Printf("Agent %s: warning: failed to populate project files: %v", sessionID, err)
	}

	// Claude runs in the repos/ subdirectory
	reposPath := filepath.Join(workspacePath, "repos")
	ctx := context.Background()

	log.Printf("Agent %s: resolved preamble (%d chars)", sessionID, len(preamble))

	// Phase 1: Planner (optional, non-fatal)
	var plan *subagent.PlanResult
	if h.config.Agent.PlannerEnabled {
		log.Printf("Agent %s: running planner", sessionID)
		planner := subagent.NewPlanner(h.executor, preamble)
		plannerState := subagent.ReadWorkspaceState(ctx, reposPath)
		plannerPromptText = planner.BuildPrompt(plannerState, message)
		log.Printf("Agent %s: planner prompt (%d chars)", sessionID, len(plannerPromptText))
		plan, err = planner.Plan(ctx, reposPath, message)
		if err != nil {
			log.Printf("Agent %s: planner failed (non-fatal): %v", sessionID, err)
		} else {
			log.Printf("Agent %s: planner produced %d steps", sessionID, len(plan.Steps))
			liveSession.SetPlanResult(plan)
		}
	}

	// Phase 2: Iteration loop with dynamic prompts
	promptBuilder := subagent.NewPromptBuilder(preamble)
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

		// Build prompt: dynamic (with plan/state) or static (backward compat)
		var systemPrompt string
		if h.config.Agent.PlannerEnabled {
			systemPrompt = promptBuilder.Build(ctx, reposPath, plan, i, message)
		} else {
			systemPrompt = promptBuilder.BuildStatic(message)
		}
		log.Printf("Agent %s: iteration %d system prompt (%d chars), message (%d chars)", sessionID, i, len(systemPrompt), len(message))

		result := h.executeIteration(ctx, reposPath, systemPrompt, message, i, deadline)
		result.Prompt = systemPrompt
		liveSession.AddIteration(result)
	}

	// Phase 3: Reviewer (optional, non-fatal)
	if h.config.Agent.ReviewerEnabled {
		log.Printf("Agent %s: running reviewer", sessionID)
		reviewer := subagent.NewReviewer(h.executor)
		review, reviewErr := reviewer.Review(ctx, reposPath, message, plan)
		if reviewErr != nil {
			log.Printf("Agent %s: reviewer failed (non-fatal): %v", sessionID, reviewErr)
		} else {
			log.Printf("Agent %s: reviewer score=%d complete=%v", sessionID, review.Score, review.Complete)
			liveSession.SetReviewResult(review)
		}
	}

	h.agentManager.CompleteSession(sessionID)
}

// resolvePrompt builds the combined system prompt from base (agent.md) and
// workflow (prompt.md) files. Returns the message directly if neither is configured.
func (h *Handlers) resolvePrompt(message string) (string, error) {
	var parts []string

	// Layer 1: Base system prompt (agent.md)
	if path := h.config.Agent.SystemPrompt; path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read system prompt %s: %w", path, err)
		}
		base := strings.TrimSpace(string(data))
		if base == "" {
			return "", fmt.Errorf("system prompt file is empty")
		}
		parts = append(parts, base)
	}

	// Layer 2: Workflow template (prompt.md) with variable substitution
	if path := h.config.Agent.PromptFile; path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read workflow prompt %s: %w", path, err)
		}
		tmpl := strings.TrimSpace(string(data))
		if tmpl == "" {
			return "", fmt.Errorf("workflow prompt file is empty")
		}
		tmpl = strings.ReplaceAll(tmpl, "{{MESSAGE}}", message)
		tmpl = strings.ReplaceAll(tmpl, "{{REPOS}}", strings.Join(h.config.Agent.SharedRepos, ", "))
		parts = append(parts, tmpl)
	}

	if len(parts) == 0 {
		return message, nil
	}

	return strings.Join(parts, "\n\n"), nil
}

// executeIteration runs a single iteration of the agent loop.
// It just runs Claude and records success or error — no git operations.
func (h *Handlers) executeIteration(
	ctx context.Context,
	workspacePath, systemPrompt, userMessage string,
	iteration int,
	deadline time.Time,
) (result agent.IterationResult) {
	iterStart := time.Now()

	result = agent.IterationResult{
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

	// Execute Claude Code with system prompt + user message
	execResult, _, execErr := h.executor.ExecuteWithLogAndSystemPrompt(iterCtx, workspacePath, systemPrompt, userMessage)
	if execErr != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("claude execution failed: %v", execErr)
		return result
	}

	if execResult != nil {
		result.Output = execResult.Output
	}
	result.Status = agent.IterationStatusSuccess
	return result
}
