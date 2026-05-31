package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/metrics"
	"github.com/agent-runner/agent-runner/internal/subagent"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

const (
	maxConsecutiveFailures = 5
	maxStuckIterations     = 5 // stop if plan progress stalls for this many iterations
)

// backoffDelay returns a delay before retrying after consecutive failures.
// 0 failures = no delay, 1 = 2s, 2 = 4s, 3 = 8s, 4 = 16s, capped at 30s.
func backoffDelay(consecutiveFails int) time.Duration {
	if consecutiveFails <= 0 {
		return 0
	}
	delay := time.Duration(1<<uint(consecutiveFails)) * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

// isPermanentError returns true for errors that cannot be resolved by retrying.
// Patterns must be specific enough to avoid misclassifying transient workspace
// errors (e.g. a missing file in the repo) as permanent CLI/auth failures.
func isPermanentError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "executable file not found in $path") ||
		strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "api key is missing") ||
		strings.Contains(lower, "api key not found") ||
		strings.Contains(lower, "no api key") ||
		strings.Contains(lower, "missing api key") ||
		strings.Contains(lower, "model not found")
}

// maxReviewerCorrections is the maximum number of corrective iterations
// the reviewer feedback loop can trigger after the main iteration loop.
const maxReviewerCorrections = 3

func (h *Handlers) executeAgent(session *agent.Session) {
	h.executeAgentWithContext(h.agentManager.Context(), session)
}

// executeAgentWithContext runs the agent iteration loop.
// The CLI handles workspace changes through the prompt; the loop manages
// planning, retries, completion criteria, and session bookkeeping.
func (h *Handlers) executeAgentWithContext(ctx context.Context, session *agent.Session) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic in agent executor: %v", r)
			slog.Error("agent goroutine panicked", "session_id", session.ID, "panic", r)
			h.agentManager.FailSession(session.ID, msg)
		}
	}()

	sessionID := session.ID
	source := session.Source
	if source == "" {
		source = "api"
	}
	message := session.Message
	maxIter := session.MaxIterations
	maxSeconds := session.MaxTotalSeconds
	authorName := session.Author

	startTime := time.Now()
	deadline := startTime.Add(time.Duration(maxSeconds) * time.Second)

	metrics.ActiveSessions.WithLabelValues(source).Inc()

	// Get the live session reference for mutations
	liveSession, _ := h.agentManager.GetSessionDirect(sessionID)
	var plannerPromptText string

	defer func() {
		metrics.ActiveSessions.WithLabelValues(source).Dec()
		metrics.QueueDepth.Set(float64(h.agentManager.QueueLength()))

		// [L13] Take a single snapshot here; the iteration loop is done so the
		// session won't change further. Reuse it for metrics, logging, and daily log.
		snap := liveSession.Snapshot()
		metrics.SessionsTotal.WithLabelValues(string(snap.Status), source).Inc()

		// Cache repos back for future runs
		if liveSession.WorkspacePath != "" {
			h.workspaceManager.CacheReposBack(liveSession.WorkspacePath, h.config.RepoCacheRoot)
		}

		// Write agent audit log
		logData := &logging.AgentLogData{
			SessionID:            sessionID,
			Status:               string(snap.Status),
			Duration:             int(time.Since(startTime).Seconds()),
			Message:              message,
			Author:               authorName,
			SuccessfulIterations: snap.SuccessfulIterations,
			TotalCostUSD:         snap.TotalCostUSD,
			Error:                snap.Error,
		}
		for _, iter := range snap.Iterations {
			logData.Iterations = append(logData.Iterations, logging.AgentIterationLog{
				Iteration:    iter.Iteration,
				Status:       string(iter.Status),
				Commit:       iter.Commit,
				ChangedFiles: iter.ChangedFiles,
				Error:        iter.Error,
				DurationSecs: iter.DurationSecs,
				CostUSD:      iter.CostUSD,
				Prompt:       iter.Prompt,
				Output:       iter.Output,
			})
		}
		logData.PlannerPrompt = plannerPromptText
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
			slog.Error("failed to write agent log", "session_id", sessionID, "error", err)
		} else {
			slog.Info("agent log written", "session_id", sessionID, "path", logFile)
		}

		// Write daily memory log with rich session details
		msgPreview := message
		if len(msgPreview) > 80 {
			msgPreview = msgPreview[:80] + "..."
		}
		var changedFiles []string
		seen := make(map[string]bool)
		for _, iter := range snap.Iterations {
			for _, f := range iter.ChangedFiles {
				if !seen[f] {
					seen[f] = true
					changedFiles = append(changedFiles, f)
				}
			}
		}
		var logLines []string
		logLines = append(logLines, fmt.Sprintf("**[%s]** %s — %d iterations, $%.4f",
			time.Now().Format("15:04"), snap.Status, snap.SuccessfulIterations, snap.TotalCostUSD))
		logLines = append(logLines, fmt.Sprintf("**Task:** %s", msgPreview))
		if len(changedFiles) > 0 {
			logLines = append(logLines, fmt.Sprintf("**Files:** %s", strings.Join(changedFiles, ", ")))
		}
		if snap.Error != "" {
			errPreview := snap.Error
			if len(errPreview) > 120 {
				errPreview = errPreview[:120] + "..."
			}
			logLines = append(logLines, fmt.Sprintf("**Error:** %s", errPreview))
		}
		dailyEntry := strings.Join(logLines, "\n")
		if err := tmpl.AppendDailyLog(h.config.MemoryDir, dailyEntry); err != nil {
			slog.Warn("failed to write daily log", "session_id", sessionID, "error", err)
			liveSession.AddWarning("daily log failed: " + err.Error())
		}

		// [H5] Retry memory push up to 3 times to survive transient network errors.
		memoryCreds := tmpl.MemoryGitCreds{
			Token:  os.Getenv("MEMORY_GIT_TOKEN"),
			User:   os.Getenv("MEMORY_GIT_USER"),
			SSHKey: os.Getenv("MEMORY_GIT_SSH_KEY"),
		}
		var pushErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(2 * time.Second)
				slog.Info("retrying memory push", "session_id", sessionID, "attempt", attempt+1)
			}
			if pushErr = tmpl.CommitAndPushMemory(h.config.MemoryDir, memoryCreds); pushErr == nil {
				break
			}
			slog.Warn("memory push attempt failed", "session_id", sessionID, "attempt", attempt+1, "error", pushErr)
		}
		if pushErr != nil {
			liveSession.AddWarning("memory push failed: " + pushErr.Error())
		}

		// Send notification to connected chat channels
		h.notifySessionResult(liveSession.Snapshot())

		// Cleanup workspace after cache-back and logging are done
		if liveSession.WorkspacePath != "" {
			h.workspaceManager.CleanupWorkspace(liveSession.WorkspacePath) //nolint:errcheck
		}
	}()

	// Auto-pull memory from git before resolving prompt (optional)
	if h.config.Agent.MemoryPullOnStart {
		creds := tmpl.MemoryGitCreds{
			Token:  os.Getenv("MEMORY_GIT_TOKEN"),
			User:   os.Getenv("MEMORY_GIT_USER"),
			SSHKey: os.Getenv("MEMORY_GIT_SSH_KEY"),
		}
		if _, err := tmpl.PullMemory(h.config.MemoryDir, creds); err != nil {
			slog.Warn("auto-pull memory failed (non-fatal)", "session_id", sessionID, "error", err)
		} else {
			slog.Info("memory pulled before session", "session_id", sessionID)
		}
	}

	// Resolve prompt: combine base system prompt (agent.md) + workflow template (prompt.md)
	if h.config.Agent.SystemPrompt != "" {
		slog.Info("system prompt configured", "session_id", sessionID, "path", h.config.Agent.SystemPrompt)
	}
	if h.config.Agent.PromptFile != "" {
		slog.Info("workflow prompt configured", "session_id", sessionID, "path", h.config.Agent.PromptFile)
	}

	preamble, err := h.resolvePrompt(message)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to resolve prompt: "+err.Error())
		return
	}

	workspacePath, missingRepos, err := h.workspaceManager.PrepareAgentWorkspace(
		h.config.RepoCacheRoot, sessionID, h.config.Agent.SharedRepos,
		h.config.Agent.SkillsDir, h.config.GitHost, h.config.GitOrg, h.config.GitToken,
	)
	if err != nil {
		h.agentManager.FailSession(sessionID, "Failed to prepare workspace: "+err.Error())
		return
	}
	// [M7] Surface missing shared repos as session warnings so the user knows
	// the agent ran with an incomplete workspace.
	for _, repo := range missingRepos {
		liveSession.AddWarning("shared repo not found in cache: " + repo)
	}
	liveSession.SetWorkspacePath(workspacePath)
	// NOTE: cleanup is done in the top-level defer (after CacheReposBack), not here.
	// A defer here would run BEFORE the earlier defer (LIFO), deleting the workspace
	// before cache-back can copy from it.

	// checkoutPath is the agent's CWD — repos, _send/, _progress.json live here
	checkoutPath := filepath.Join(workspacePath, "workspace")

	slog.Info("resolved preamble", "session_id", sessionID, "chars", len(preamble))

	// Phase 1: Planner (optional, non-fatal). Runs regardless of CLI backend.
	var plan *subagent.PlanResult
	if h.config.Agent.PlannerEnabled {
		slog.Info("running planner", "session_id", sessionID)
		planner := subagent.NewPlanner(h.plannerClient, preamble)
		plannerState := subagent.ReadWorkspaceState(ctx, checkoutPath)
		plannerPromptText = planner.BuildPrompt(plannerState, message)
		slog.Info("planner prompt built", "session_id", sessionID, "chars", len(plannerPromptText))
		plan, err = planner.Plan(ctx, checkoutPath, message)
		if err != nil {
			if isPermanentError(err.Error()) {
				slog.Error("aborting: permanent planner error", "session_id", sessionID, "error", err)
				h.agentManager.FailSession(sessionID, err.Error())
				return
			}
			// [M8] Non-permanent planner failure: fall through to the executor without
			// a plan, and record a warning so the user knows planning was skipped.
			slog.Warn("planner failed — falling through to executor", "session_id", sessionID, "error", err)
			liveSession.AddWarning("planner failed: " + err.Error())
		} else {
			slog.Info("planner produced steps", "session_id", sessionID, "steps", len(plan.Steps))
			liveSession.SetPlanResult(plan, len(plan.Steps))
		}
	}

	// Phase 2: Iteration loop with dynamic prompts
	promptBuilder := subagent.NewPromptBuilder(preamble)
	iterReason := "first iteration"
	stopReason := fmt.Sprintf("reached max iterations (%d)", maxIter)
	completed := false
	blockedOrStuck := false
	iterationsRun := 0
	lastProgressCount := -1 // -1 = no progress file seen yet
	stuckCount := 0
	for i := 1; i <= maxIter; i++ {
		// Check stop signal or context cancellation (server shutdown)
		if liveSession.StopRequested() {
			stopReason = "stop requested"
			slog.Info("stop requested", "session_id", sessionID, "after_iteration", i-1)
			break
		}
		if ctx.Err() != nil {
			stopReason = "context cancelled"
			slog.Info("context cancelled", "session_id", sessionID, "after_iteration", i-1)
			break
		}

		// Check time limit
		if time.Now().After(deadline) {
			stopReason = fmt.Sprintf("time limit reached (%ds)", maxSeconds)
			slog.Info("time limit reached", "session_id", sessionID, "max_seconds", maxSeconds)
			break
		}

		// Check consecutive failures
		consecutiveFails := liveSession.GetConsecutiveFailures()
		if consecutiveFails >= maxConsecutiveFailures {
			_, lastErr, _ := liveSession.LastIterationError()
			failMsg := fmt.Sprintf("aborted after %d consecutive failures", consecutiveFails)
			if lastErr != "" {
				failMsg += ": " + lastErr
			}
			slog.Error("aborting session", "session_id", sessionID, "reason", failMsg)
			h.agentManager.FailSession(sessionID, failMsg)
			return
		}

		// Exponential backoff on consecutive failures
		if delay := backoffDelay(consecutiveFails); delay > 0 {
			slog.Info("backing off before retry", "session_id", sessionID, "delay", delay, "consecutive_failures", consecutiveFails)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				stopReason = "context cancelled"
			}
			if ctx.Err() != nil {
				break
			}
		}

		// Build error context from previous iteration failure (if any)
		errorContext := ""
		if iterNum, errMsg, partialOut := liveSession.LastIterationError(); errMsg != "" {
			errorContext = buildErrorContext(iterNum, errMsg, partialOut)
		}

		// Build prompt: dynamic (with plan/state) or static (backward compat)
		var systemPrompt string
		if h.config.Agent.PlannerEnabled {
			systemPrompt = promptBuilder.Build(ctx, checkoutPath, plan, i, message, errorContext)
		} else {
			systemPrompt = promptBuilder.BuildStatic(message, errorContext)
		}
		slog.Info("starting iteration", "session_id", sessionID, "iteration", i, "reason", iterReason, "prompt_chars", len(systemPrompt), "message_chars", len(message))

		liveSession.BeginIteration(i)
		result := h.executeIteration(ctx, checkoutPath, systemPrompt, message, i, deadline, h.getExecutor())
		result.Prompt = systemPrompt
		result.Retry = errorContext != ""
		iterationsRun = i

		// Check if the agent signalled task completion; strip the marker from output.
		taskDone := false
		if result.Output != "" {
			result.Output, taskDone = subagent.ParseDoneSignal(result.Output)
		}

		liveSession.AddIteration(result)

		if result.Status == agent.IterationStatusError && isPermanentError(result.Error) {
			slog.Error("aborting: permanent error", "session_id", sessionID, "iteration", i, "error", result.Error)
			h.agentManager.FailSession(sessionID, result.Error)
			return
		}

		metrics.IterationsTotal.WithLabelValues(string(result.Status), source).Inc()
		metrics.IterationDurationSeconds.WithLabelValues(source).Observe(float64(result.DurationSecs))
		if result.CostUSD > 0 {
			metrics.CostUSDTotal.WithLabelValues(source).Add(result.CostUSD)
		}

		if taskDone {
			stopReason = "task complete"
			completed = true
			slog.Info("agent signalled task complete", "session_id", sessionID, "iteration", i)
			break
		}

		// Update completed steps from progress file and sync to plan
		progress := subagent.ReadProgress(checkoutPath)
		if len(progress.BlockedSteps) > 0 {
			reasons := make([]string, len(progress.BlockedSteps))
			for j, b := range progress.BlockedSteps {
				reasons[j] = fmt.Sprintf("step %s: %s", b.Step, b.Reason)
			}
			stopReason = "blocked: " + strings.Join(reasons, "; ")
			blockedOrStuck = true
			slog.Info("agent blocked on plan steps", "session_id", sessionID, "iteration", i, "reason", stopReason)
			break
		}
		if len(progress.CompletedSteps) > 0 {
			liveSession.SetCompletedSteps(progress.CompletedSteps)
			if plan != nil {
				plan.MarkDone(progress.CompletedSteps)
				if len(plan.RemainingSteps()) == 0 {
					stopReason = "all plan steps completed"
					completed = true
					slog.Info("all plan steps completed", "session_id", sessionID, "iteration", i)
					break
				}
			}
			// Stuck detection: stop if plan progress hasn't advanced for maxStuckIterations.
			cur := len(progress.CompletedSteps)
			if cur == lastProgressCount {
				stuckCount++
				if stuckCount >= maxStuckIterations {
					stopReason = fmt.Sprintf("no plan progress for %d iterations (completed %d steps)", stuckCount, cur)
					blockedOrStuck = true
					slog.Info("stopping stuck session", "session_id", sessionID, "iteration", i, "completed_steps", cur)
					break
				}
			} else {
				stuckCount = 0
				lastProgressCount = cur
			}
			iterReason = "plan steps remaining"
		} else if result.Status == agent.IterationStatusError {
			iterReason = fmt.Sprintf("retry after error: %s", result.Error)
		} else {
			iterReason = "task not yet signalled done"
		}
	}
	slog.Info("iteration loop finished", "session_id", sessionID, "stop_reason", stopReason, "iterations", iterationsRun, "elapsed_secs", int(time.Since(startTime).Seconds()))

	// Phase 3: Reviewer with feedback loop (optional, non-fatal)
	// Skip reviewer when the session stopped because the agent was blocked or stuck —
	// reviewing incomplete work isn't useful and wastes tokens.
	if h.config.Agent.ReviewerEnabled && !blockedOrStuck {
		reviewer := subagent.NewReviewer(h.getExecutor())

		for correction := 0; correction <= maxReviewerCorrections; correction++ {
			if liveSession.StopRequested() || ctx.Err() != nil || time.Now().After(deadline) {
				break
			}

			slog.Info("running reviewer", "session_id", sessionID, "pass", correction)
			review, reviewErr := reviewer.Review(ctx, checkoutPath, message, plan)
			if reviewErr != nil {
				slog.Warn("reviewer failed (non-fatal)", "session_id", sessionID, "error", reviewErr)
				break
			}

			slog.Info("reviewer completed", "session_id", sessionID,
				"score", review.Score, "complete", review.Complete,
				"issues", len(review.Issues), "pass", correction)
			liveSession.SetReviewResult(review)

			// If complete or no issues, we're done
			if review.Complete || len(review.Issues) == 0 {
				break
			}

			// Don't run corrective iterations on the last pass
			if correction >= maxReviewerCorrections {
				slog.Info("reviewer correction limit reached", "session_id", sessionID)
				break
			}

			// Run a corrective iteration with reviewer feedback as context
			correctionContext := buildReviewerContext(review)
			iterNum := liveSession.Snapshot().CurrentIteration + 1

			var systemPrompt string
			if h.config.Agent.PlannerEnabled {
				systemPrompt = promptBuilder.Build(ctx, checkoutPath, plan, iterNum, message, correctionContext)
			} else {
				systemPrompt = promptBuilder.BuildStatic(message, correctionContext)
			}

			slog.Info("running corrective iteration", "session_id", sessionID,
				"iteration", iterNum, "issues", len(review.Issues))

			result := h.executeIteration(ctx, checkoutPath, systemPrompt, message, iterNum, deadline, h.getExecutor())
			result.Prompt = systemPrompt
			result.Retry = true
			liveSession.AddIteration(result)

			metrics.IterationsTotal.WithLabelValues(string(result.Status), source).Inc()
			metrics.IterationDurationSeconds.WithLabelValues(source).Observe(float64(result.DurationSecs))
			if result.CostUSD > 0 {
				metrics.CostUSDTotal.WithLabelValues(source).Add(result.CostUSD)
			}

			// [M9] Stop corrective iterations if the workspace is broken — further
			// corrections will fail for the same reason.
			if result.Status == agent.IterationStatusError {
				slog.Warn("corrective iteration failed, stopping reviewer loop",
					"session_id", sessionID, "iteration", iterNum, "error", result.Error)
				break
			}

			// Update progress after correction
			if p := subagent.ReadProgress(checkoutPath); len(p.CompletedSteps) > 0 {
				liveSession.SetCompletedSteps(p.CompletedSteps)
				if plan != nil {
					plan.MarkDone(p.CompletedSteps)
				}
			}
		}
	}

	// If the agent committed but didn't push, try to push now and warn on failure.
	if pushWarn := pushUnpushedCommits(ctx, checkoutPath, h.config.GitPushRetries, h.config.GitPushRetryDelaySeconds); pushWarn != "" {
		slog.Warn("unpushed commits after agent completed", "session_id", sessionID, "warning", pushWarn)
		liveSession.AddWarning(pushWarn)
	}

	// Collect output files from _send/ after all phases (including reviewer corrections).
	sendDir := filepath.Join(checkoutPath, "_send")
	if outputFiles, err := collectOutputFiles(sendDir); err != nil {
		slog.Warn("failed to collect _send/ files", "session_id", sessionID, "error", err)
	} else if len(outputFiles) > 0 {
		slog.Info("collected output files", "session_id", sessionID, "count", len(outputFiles))
		liveSession.SetOutputFiles(outputFiles)
		if h.config.OutputsRoot != "" {
			if err := persistOutputFiles(sendDir, h.config.OutputsRoot, sessionID); err != nil {
				slog.Warn("failed to persist output files", "session_id", sessionID, "error", err)
			}
		}
	}

	// Collect and submit scheduled tasks from _schedule.json.
	if schedEntries, err := collectScheduleEntries(checkoutPath); err != nil {
		slog.Warn("failed to collect _schedule.json", "session_id", sessionID, "error", err)
	} else if len(schedEntries) > 0 {
		if h.workflowClient != nil {
			slog.Info("submitting schedule entries", "session_id", sessionID, "count", len(schedEntries))
			if err := h.workflowClient.SubmitSchedule(ctx, schedEntries, h.config.Runner.TypePrefix); err != nil {
				slog.Warn("failed to submit schedule entries", "session_id", sessionID, "error", err)
			}
		} else {
			slog.Warn("schedule entries found but no workflow client configured", "session_id", sessionID, "count", len(schedEntries))
		}
	}

	if completed {
		h.agentManager.CompleteSession(sessionID)
		return
	}
	if liveSession.StopRequested() {
		h.agentManager.CompleteSession(sessionID)
		return
	}
	if ctx.Err() != nil {
		h.agentManager.FailSession(sessionID, "context cancelled before completion")
		return
	}
	if strings.HasPrefix(stopReason, "time limit reached") {
		h.agentManager.FailSession(sessionID, stopReason)
		return
	}
	if blockedOrStuck {
		h.agentManager.FailSession(sessionID, stopReason)
		return
	}
	snap := liveSession.Snapshot()
	if len(snap.Iterations) > 0 {
		last := snap.Iterations[len(snap.Iterations)-1]
		if last.Status == agent.IterationStatusSuccess || last.Status == agent.IterationStatusNoChanges {
			h.agentManager.CompleteSession(sessionID)
			return
		}
	}
	h.agentManager.FailSession(sessionID, stopReason)
}

// resolvePrompt builds the combined system prompt using the new single-agent
// memory architecture: system instructions → memory sections → current request.
func (h *Handlers) resolvePrompt(message string) (string, error) {
	systemPromptPath, promptFilePath := h.bootstrapPaths()

	// Read system instructions (agent.md); empty string if file missing — no error.
	var systemInstructions string
	if data, err := os.ReadFile(systemPromptPath); err == nil {
		systemInstructions = strings.TrimSpace(string(data))
	}

	// Append prompt file (prompt.md) if non-empty.
	if data, err := os.ReadFile(promptFilePath); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			if systemInstructions != "" {
				systemInstructions += "\n\n" + s
			} else {
				systemInstructions = s
			}
		}
	}

	// Retrieve memory sections.
	retrieval := tmpl.Retrieve(h.config.MemoryDir)
	if len(retrieval.Files) > 0 {
		names := make([]string, len(retrieval.Files))
		for i, f := range retrieval.Files {
			names[i] = f.Name
		}
		slog.Info("memory loaded", "files", len(retrieval.Files), "sections", strings.Join(names, ", "))
	} else {
		slog.Info("memory loaded", "files", 0)
	}

	// Build vars map.
	runnerURL := "http://" + h.config.API.Bind
	absMemoryDir, _ := filepath.Abs(h.config.MemoryDir)
	vars := map[string]string{
		tmpl.VarMessage:    message,
		tmpl.VarDate:       time.Now().Format("2006-01-02"),
		tmpl.VarRunnerURL:  runnerURL,
		tmpl.VarAPIKey:     h.config.API.APIKey,
		tmpl.VarRepos:      strings.Join(h.config.Agent.SharedRepos, ", "),
		tmpl.VarProjectDir: h.config.ProjectDir,
		tmpl.VarMemoryDir:  absMemoryDir,
	}

	input := tmpl.PromptInput{
		SystemInstructions: systemInstructions,
		Retrieval:          retrieval,
		CurrentRequest:     message,
		Vars:               vars,
	}

	return tmpl.Compile(input), nil
}

// executeIteration runs a single iteration of the agent loop.
// It just runs Claude and records success or error — no git operations.
func (h *Handlers) executeIteration(
	ctx context.Context,
	workspacePath, systemPrompt, userMessage string,
	iteration int,
	deadline time.Time,
	exec executor.Executor,
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

	// Execute the configured agent CLI with system prompt + user message.
	execResult, _, execErr := exec.ExecuteWithLogAndSystemPrompt(iterCtx, workspacePath, systemPrompt, userMessage)
	if execErr != nil {
		result.Status = agent.IterationStatusError
		result.Error = fmt.Sprintf("agent execution failed: %v", execErr)
		if execResult != nil {
			// Prefer structured output; fall back to raw terminal output so the
			// full CLI error appears in the audit log's details block.
			if execResult.Output != "" {
				result.Output = execResult.Output
			} else if execResult.RawOutput != "" {
				result.Output = execResult.RawOutput
			}
			result.CostUSD = execResult.CostUSD
		}
		return result
	}

	if execResult != nil {
		result.Output = execResult.Output
		result.CostUSD = execResult.CostUSD
	}
	result.Status = agent.IterationStatusSuccess
	return result
}

const (
	maxOutputFiles     = 20
	maxOutputTotalSize = 10 << 20 // 10MB
)

// collectOutputFiles reads files from the _send/ directory and returns them
// as OutputFile slices. Caps at maxOutputFiles files and maxOutputTotalSize total bytes.
func collectOutputFiles(sendDir string) ([]agent.OutputFile, error) {
	entries, err := os.ReadDir(sendDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read _send/ dir: %w", err)
	}

	var files []agent.OutputFile
	var totalSize int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if len(files) >= maxOutputFiles {
			slog.Warn("output file limit reached, skipping remaining", "limit", maxOutputFiles)
			break
		}

		path := filepath.Join(sendDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			slog.Warn("skipping output file", "file", entry.Name(), "error", err)
			continue
		}

		if totalSize+info.Size() > maxOutputTotalSize {
			slog.Warn("skipping output file, would exceed size limit", "file", entry.Name(), "limit_mb", maxOutputTotalSize>>20)
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("skipping output file", "file", entry.Name(), "error", err)
			continue
		}

		contentType := http.DetectContentType(data)
		totalSize += int64(len(data))

		files = append(files, agent.OutputFile{
			Name:        entry.Name(),
			Data:        data,
			ContentType: contentType,
		})
	}

	return files, nil
}

// persistOutputFiles copies files from sendDir to outputsRoot/<date>/<sessionID>/
// so they survive workspace cleanup and are accessible to subsequent sessions.
func persistOutputFiles(sendDir, outputsRoot, sessionID string) error {
	entries, err := os.ReadDir(sendDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	date := time.Now().UTC().Format("2006-01-02")
	destDir := filepath.Join(outputsRoot, date, sessionID)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src := filepath.Join(sendDir, entry.Name())
		dst := filepath.Join(destDir, entry.Name())
		if err := copyFile(src, dst); err != nil {
			slog.Warn("failed to persist output file", "file", entry.Name(), "error", err)
		} else {
			slog.Info("persisted output file", "path", dst)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// notifySessionResult sends the agent session result to connected chat channels.
// Called from executeAgent's defer so both runner-initiated and API-initiated sessions
// get notifications. Runner bridge also calls its own notify (which is fine — the
// runner bridge notify is richer for runner tasks; this one is the catch-all).
func (h *Handlers) notifySessionResult(snap *agent.Session) {
	if h.notifier == nil {
		return
	}

	// Chat-initiated sessions (stream/telegram/wechat) are already watched by a
	// pollAndReport goroutine that delivers the result directly to the originating
	// conversation. Sending a second notification via SendMessage causes the bot to
	// receive its own message as user input, set pendingInput=true, and re-run the
	// same task after the session completes.
	switch snap.Source {
	case "stream", "telegram", "wechat":
		return
	}

	preview := snap.Message
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}

	var msg string
	switch snap.Status {
	case agent.SessionStatusCompleted:
		if output := lastAgentOutput(snap); output != "" {
			msg = output
		} else {
			msg = fmt.Sprintf("Agent completed\n• Session: %s\n• Message: %s\n• Iterations: %d\n• Duration: %ds",
				snap.ID, preview, snap.SuccessfulIterations, snap.ElapsedSeconds)
		}
	case agent.SessionStatusFailed:
		errPreview := snap.Error
		if len(errPreview) > 120 {
			errPreview = errPreview[:120] + "..."
		}
		if output := lastAgentOutput(snap); output != "" {
			msg = fmt.Sprintf("%s\n\n---\nAgent failed — %s", output, errPreview)
		} else {
			msg = fmt.Sprintf("Agent failed\n• Session: %s\n• Message: %s\n• Error: %s",
				snap.ID, preview, errPreview)
		}
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.notifier.SendNotification(ctx, msg); err != nil {
		slog.Error("notification failed", "session_id", snap.ID, "error", err)
	} else {
		slog.Info("notification sent", "session_id", snap.ID)
	}
}

// lastAgentOutput returns the output from the last iteration with output.
func lastAgentOutput(snap *agent.Session) string {
	for i := len(snap.Iterations) - 1; i >= 0; i-- {
		if snap.Iterations[i].Output != "" {
			return snap.Iterations[i].Output
		}
	}
	return ""
}

// buildReviewerContext formats reviewer issues and suggestions as markdown
// context for a corrective iteration.
func buildReviewerContext(review *subagent.ReviewResult) string {
	var sb strings.Builder
	sb.WriteString("## Reviewer Feedback (corrective iteration)\n\n")
	sb.WriteString(fmt.Sprintf("**Score:** %d/10\n\n", review.Score))
	if len(review.Issues) > 0 {
		sb.WriteString("**Issues to fix:**\n")
		for _, issue := range review.Issues {
			sb.WriteString(fmt.Sprintf("- %s\n", issue))
		}
		sb.WriteString("\n")
	}
	if len(review.Suggestions) > 0 {
		sb.WriteString("**Suggestions:**\n")
		for _, s := range review.Suggestions {
			sb.WriteString(fmt.Sprintf("- %s\n", s))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Please address the issues above.\n")
	return sb.String()
}

const maxPartialOutputChars = 2000

// pushUnpushedCommits checks whether the workspace has commits that haven't
// been pushed to origin (i.e. the agent committed but forgot to push). If any
// are found it attempts a push and returns a warning string on failure.
// Returns "" when everything is in sync or when there is no upstream tracking
// branch (in which case a push wouldn't be meaningful).
func pushUnpushedCommits(ctx context.Context, repoPath string, retries, retryDelay int) string {
	// git log @{u}..HEAD lists commits ahead of the upstream tracking branch.
	// An error here means no upstream is configured — nothing to do.
	cmd := gitCmd(ctx, repoPath, "log", "@{u}..HEAD", "--oneline")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	// There are unpushed commits — try to push.
	ops := gitpkg.NewOperations(retries, retryDelay)
	if err := ops.Push(ctx, repoPath); err != nil {
		return fmt.Sprintf("agent committed but git push failed: %v", err)
	}
	return ""
}

func gitCmd(ctx context.Context, repoPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath
	return cmd
}

// buildErrorContext formats the last iteration error and partial output as
// markdown so Claude can see what went wrong and self-correct.
func buildErrorContext(iterNum int, errMsg, partialOutput string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Previous Iteration Error (iteration %d)\n\n", iterNum))
	sb.WriteString(fmt.Sprintf("**Error:** %s\n", errMsg))
	if partialOutput != "" {
		truncated := partialOutput
		if len(truncated) > maxPartialOutputChars {
			truncated = truncated[:maxPartialOutputChars] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("\n**Partial output:**\n```\n%s\n```\n", truncated))
	}
	return sb.String()
}
