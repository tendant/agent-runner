package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/subagent"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
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

		// Write agent audit log
		snap := liveSession.Snapshot()
		logData := &logging.AgentLogData{
			SessionID:    sessionID,
			Status:       string(snap.Status),
			Duration:     int(time.Since(startTime).Seconds()),
			Message:      message,
			Author:       authorName,
			SuccessfulIterations: snap.SuccessfulIterations,
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

		// Write daily memory log
		snap2 := liveSession.Snapshot()
		msgPreview := message
		if len(msgPreview) > 80 {
			msgPreview = msgPreview[:80] + "..."
		}
		dailyEntry := fmt.Sprintf("[%s] session %s: %s, %d iterations — %s",
			time.Now().Format("15:04"), sessionID, snap2.Status, snap2.SuccessfulIterations, msgPreview)
		if err := tmpl.AppendDailyLog(h.config.MemoryDir, dailyEntry); err != nil {
			log.Printf("Agent %s: warning: failed to write daily log: %v", sessionID, err)
		}

		// Complete bootstrap lifecycle (rename BOOTSTRAP.md → .done)
		if liveSession.Status == agent.SessionStatusCompleted {
			if err := tmpl.CompleteBootstrap(h.config.TemplatesDir); err != nil {
				log.Printf("Agent %s: warning: bootstrap completion failed: %v", sessionID, err)
			}
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

	// Seed memory from templates on first run
	if err := tmpl.SeedMemory(h.config.TemplatesDir, h.config.MemoryDir); err != nil {
		log.Printf("Agent %s: warning: failed to seed memory: %v", sessionID, err)
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

	// Claude runs in the repos/ subdirectory
	reposPath := filepath.Join(workspacePath, "repos")
	ctx := h.agentManager.Context()

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
		// Check stop signal or context cancellation (server shutdown)
		if liveSession.StopRequested() {
			log.Printf("Agent %s: stop requested, exiting after iteration %d", sessionID, i-1)
			break
		}
		if ctx.Err() != nil {
			log.Printf("Agent %s: context cancelled, exiting after iteration %d", sessionID, i-1)
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
			_, lastErr, _ := liveSession.LastIterationError()
			failMsg := fmt.Sprintf("aborted after %d consecutive failures", consecutiveFails)
			if lastErr != "" {
				failMsg += ": " + lastErr
			}
			log.Printf("Agent %s: %s", sessionID, failMsg)
			h.agentManager.FailSession(sessionID, failMsg)
			return
		}

		// Build error context from previous iteration failure (if any)
		errorContext := ""
		if iterNum, errMsg, partialOut := liveSession.LastIterationError(); errMsg != "" {
			errorContext = buildErrorContext(iterNum, errMsg, partialOut)
		}

		// Build prompt: dynamic (with plan/state) or static (backward compat)
		var systemPrompt string
		if h.config.Agent.PlannerEnabled {
			systemPrompt = promptBuilder.Build(ctx, reposPath, plan, i, message, errorContext)
		} else {
			systemPrompt = promptBuilder.BuildStatic(message, errorContext)
		}
		log.Printf("Agent %s: iteration %d system prompt (%d chars), message (%d chars)", sessionID, i, len(systemPrompt), len(message))

		result := h.executeIteration(ctx, reposPath, systemPrompt, message, i, deadline)
		result.Prompt = systemPrompt
		result.Retry = errorContext != ""
		liveSession.AddIteration(result)

		// Update completed steps from progress file
		if completedSteps := subagent.ReadProgress(reposPath); len(completedSteps) > 0 {
			liveSession.SetCompletedSteps(completedSteps)
		}
	}

	// Collect output files from _send/ directory
	sendDir := filepath.Join(reposPath, "_send")
	if outputFiles, err := collectOutputFiles(sendDir); err != nil {
		log.Printf("Agent %s: warning: failed to collect _send/ files: %v", sessionID, err)
	} else if len(outputFiles) > 0 {
		log.Printf("Agent %s: collected %d output files from _send/", sessionID, len(outputFiles))
		liveSession.SetOutputFiles(outputFiles)
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

// resolvePrompt builds the combined system prompt using the template system
// (embedded defaults + optional user overrides) plus legacy prompt files for
// backward compatibility.
func (h *Handlers) resolvePrompt(message string) (string, error) {
	return h.resolveTemplatePrompt(message)
}

// resolveTemplatePrompt composes the prompt from the template system and
// appends legacy AGENT_SYSTEM_PROMPT / AGENT_PROMPT_FILE content if set.
func (h *Handlers) resolveTemplatePrompt(message string) (string, error) {
	ctx := tmpl.NewContext(message, h.config.Agent.SharedRepos, 1)
	templatesDir := h.config.TemplatesDir

	// Check for bootstrap (first_run)
	firstRun := tmpl.IsFirstRun(templatesDir)

	// Compose from embedded defaults + user overrides
	composed, err := tmpl.ComposePrompt(templatesDir, tmpl.PhaseBoot, firstRun, ctx)
	if err != nil {
		return "", fmt.Errorf("template composition failed: %w", err)
	}

	var parts []string
	if composed != "" {
		parts = append(parts, composed)
	}

	// Legacy: append AGENT_SYSTEM_PROMPT if set (backward compat)
	if path := h.config.Agent.SystemPrompt; path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read system prompt %s: %w", path, err)
		}
		base := strings.TrimSpace(string(data))
		if base != "" {
			parts = append(parts, base)
		}
	}

	// Legacy: append AGENT_PROMPT_FILE if set (backward compat)
	if path := h.config.Agent.PromptFile; path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read workflow prompt %s: %w", path, err)
		}
		wf := strings.TrimSpace(string(data))
		if wf != "" {
			wf = strings.ReplaceAll(wf, "{{MESSAGE}}", message)
			wf = strings.ReplaceAll(wf, "{{REPOS}}", strings.Join(h.config.Agent.SharedRepos, ", "))
			parts = append(parts, wf)
		}
	}

	// Append memory section
	memorySec := tmpl.ComposeMemorySection(h.config.MemoryDir, h.config.Agent.MemoryDays)
	if memorySec != "" {
		parts = append(parts, memorySec)
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
		// Preserve partial output so it can be fed back into the next iteration
		if execResult != nil && execResult.Output != "" {
			result.Output = execResult.Output
		} else if execResult != nil && execResult.RawOutput != "" {
			result.Output = execResult.RawOutput
		}
		return result
	}

	if execResult != nil {
		result.Output = execResult.Output
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
			log.Printf("collectOutputFiles: hit file limit (%d), skipping remaining", maxOutputFiles)
			break
		}

		path := filepath.Join(sendDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			log.Printf("collectOutputFiles: skip %s: %v", entry.Name(), err)
			continue
		}

		if totalSize+info.Size() > maxOutputTotalSize {
			log.Printf("collectOutputFiles: skip %s: would exceed %dMB total limit", entry.Name(), maxOutputTotalSize>>20)
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("collectOutputFiles: skip %s: %v", entry.Name(), err)
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

const maxPartialOutputChars = 2000

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
