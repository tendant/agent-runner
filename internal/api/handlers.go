package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// Handlers contains all HTTP handlers
type Handlers struct {
	config           *config.Config
	jobManager       *jobs.Manager
	gitOps           *git.Operations
	executor         *executor.Executor
	validator        *executor.Validator
	workspaceManager *executor.WorkspaceManager
	runLogger        *logging.RunLogger
}

// NewHandlers creates a new handlers instance
func NewHandlers(
	cfg *config.Config,
	jobManager *jobs.Manager,
	gitOps *git.Operations,
	exec *executor.Executor,
	validator *executor.Validator,
	workspaceManager *executor.WorkspaceManager,
	runLogger *logging.RunLogger,
) *Handlers {
	return &Handlers{
		config:           cfg,
		jobManager:       jobManager,
		gitOps:           gitOps,
		executor:         exec,
		validator:        validator,
		workspaceManager: workspaceManager,
		runLogger:        runLogger,
	}
}

// RunRequest represents the POST /run request body
type RunRequest struct {
	Project       string   `json:"project"`
	Instruction   string   `json:"instruction"`
	Paths         []string `json:"paths"`
	CommitMessage string   `json:"commit_message,omitempty"`
	Author        string   `json:"author,omitempty"`
}

// HandleRun handles POST /run - initiate a Claude Code execution
func (h *Handlers) HandleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Project == "" {
		h.writeError(w, http.StatusBadRequest, "project is required")
		return
	}
	if req.Instruction == "" {
		h.writeError(w, http.StatusBadRequest, "instruction is required")
		return
	}
	if len(req.Paths) == 0 {
		h.writeError(w, http.StatusBadRequest, "paths is required")
		return
	}

	// Check if project is allowed
	if !h.config.IsProjectAllowed(req.Project) {
		h.writeError(w, http.StatusBadRequest, "project not in allowed_projects")
		return
	}

	// Check if project exists
	projectPath := filepath.Join(h.config.ProjectsRoot, req.Project)
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		h.writeError(w, http.StatusBadRequest, "project directory not found")
		return
	}

	// Set default author
	author := req.Author
	if author == "" {
		author = "claude-bot"
	}

	// Create job
	job, err := h.jobManager.CreateJob(req.Project, req.Instruction, req.Paths, req.CommitMessage, author)
	if err != nil {
		if strings.Contains(err.Error(), "locked") {
			h.writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error": err.Error(),
			})
			return
		}
		if strings.Contains(err.Error(), "capacity") {
			h.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
				"error": err.Error(),
			})
			return
		}
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Start execution in background
	go h.executeJob(job, projectPath)

	// Return 202 Accepted with job info
	h.writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":  job.ID,
		"status":  job.Status,
		"project": job.Project,
	})
}

// HandleGetJob handles GET /job/{job_id}
func (h *Handlers) HandleGetJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Extract job ID from path
	jobID := strings.TrimPrefix(r.URL.Path, "/job/")
	if jobID == "" {
		h.writeError(w, http.StatusBadRequest, "job_id is required")
		return
	}

	job, exists := h.jobManager.GetJob(jobID)
	if !exists {
		h.writeError(w, http.StatusNotFound, "job not found")
		return
	}

	h.writeJSON(w, http.StatusOK, job.ToResponse())
}

// HandleGetStatus handles GET /status/{project}
func (h *Handlers) HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	project := strings.TrimPrefix(r.URL.Path, "/status/")
	if project == "" {
		h.writeError(w, http.StatusBadRequest, "project is required")
		return
	}

	h.writeJSON(w, http.StatusOK, h.jobManager.GetProjectStatus(project))
}

// HandleGetProjects handles GET /projects
func (h *Handlers) HandleGetProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Get list of actual projects from disk
	var projects []string
	entries, err := os.ReadDir(h.config.ProjectsRoot)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				// Check if it's a git repo
				gitPath := filepath.Join(h.config.ProjectsRoot, entry.Name(), ".git")
				if _, err := os.Stat(gitPath); err == nil {
					if h.config.IsProjectAllowed(entry.Name()) {
						projects = append(projects, entry.Name())
					}
				}
			}
		}
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"projects": h.jobManager.ListProjects(projects),
	})
}

// executeJob runs the full job execution pipeline
func (h *Handlers) executeJob(job *jobs.Job, projectPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(h.config.MaxRuntimeSeconds)*time.Second)
	defer cancel()

	startTime := time.Now()

	logData := &logging.RunLogData{
		JobID:       job.ID,
		Project:     job.Project,
		Instruction: job.Instruction,
	}

	defer func() {
		// Always write run log
		logData.Duration = int(time.Since(startTime).Seconds())
		logFile, _ := h.runLogger.WriteRunLog(logData)

		if job.Result != nil {
			job.Result.LogFile = logFile
		}
	}()

	// Update status to running
	h.jobManager.UpdateStatus(job.ID, jobs.StatusRunning)

	// Step 1: Fetch and reset the source project
	if err := h.gitOps.FetchAndReset(ctx, projectPath); err != nil {
		h.failJob(job, logData, "Failed to prepare git repository: "+err.Error(), "GIT_NETWORK_ERROR")
		return
	}

	// Step 2: Prepare workspace
	workspacePath, err := h.workspaceManager.PrepareWorkspace(projectPath, job.ID)
	if err != nil {
		h.failJob(job, logData, "Failed to prepare workspace: "+err.Error(), "")
		return
	}
	h.jobManager.SetWorkspacePath(job.ID, workspacePath)
	defer h.workspaceManager.CleanupWorkspace(workspacePath)

	// Step 3: Execute Claude Code
	result, executionLog, err := h.executor.ExecuteWithLog(ctx, workspacePath, job.Instruction)
	logData.ExecutionLog = executionLog

	if err != nil {
		errorCode := "CLAUDE_ERROR"
		if strings.Contains(err.Error(), "TIMEOUT") {
			errorCode = "TIMEOUT"
		}
		h.failJob(job, logData, err.Error(), errorCode)
		return
	}
	_ = result // result.Output available if needed

	// Step 4: Get changed files
	changedFiles, err := h.gitOps.GetChangedFiles(ctx, workspacePath)
	if err != nil {
		h.failJob(job, logData, "Failed to get changed files: "+err.Error(), "")
		return
	}

	if len(changedFiles) == 0 {
		h.failJob(job, logData, "No changes were made by Claude Code", "")
		return
	}

	// Populate log data with changed files
	for _, f := range changedFiles {
		logData.ChangedFiles = append(logData.ChangedFiles, logging.FileChange{Path: f})
	}

	// Step 5: Validate diff
	h.jobManager.UpdateStatus(job.ID, jobs.StatusValidating)

	validationErr := h.validator.ValidateDiff(changedFiles, job.Paths)
	if validationErr != nil {
		logData.ValidationOK = false
		logData.ValidationError = &logging.ValidationResult{
			Code:    validationErr.Code,
			Message: validationErr.Message,
			Files:   validationErr.Files,
		}
		h.failJob(job, logData, validationErr.Message, validationErr.Code)
		return
	}
	logData.ValidationOK = true

	// Step 6: Get diff summary
	diffSummary, _ := h.gitOps.GetDiffSummary(ctx, workspacePath)
	logData.DiffSummary = logging.DiffSummary{
		Insertions: diffSummary.Insertions,
		Deletions:  diffSummary.Deletions,
	}

	// Step 7: Commit
	h.jobManager.UpdateStatus(job.ID, jobs.StatusPushing)

	commitMessage := job.CommitMessage
	if commitMessage == "" {
		commitMessage = h.generateCommitMessage(changedFiles, job.Instruction)
	}

	commitHash, err := h.gitOps.Commit(ctx, workspacePath, commitMessage, job.Author, job.Instruction)
	if err != nil {
		h.failJob(job, logData, "Failed to commit: "+err.Error(), "")
		return
	}
	logData.Commit = commitHash

	// Step 8: Push
	if err := h.gitOps.Push(ctx, workspacePath); err != nil {
		errorCode := ""
		if strings.Contains(err.Error(), "GIT_PUSH_CONFLICT") {
			errorCode = "GIT_PUSH_CONFLICT"
		} else if strings.Contains(err.Error(), "GIT_AUTH_FAILURE") {
			errorCode = "GIT_AUTH_FAILURE"
		} else {
			errorCode = "GIT_NETWORK_ERROR"
		}
		h.failJob(job, logData, err.Error(), errorCode)
		return
	}

	// Success!
	logData.Status = "completed"
	branch, _ := h.gitOps.GetCurrentBranch(ctx, workspacePath)
	logData.Branch = branch

	h.jobManager.SetJobResult(job.ID, &jobs.JobResult{
		Commit:       commitHash,
		ChangedFiles: changedFiles,
		DiffSummary: jobs.DiffSummary{
			Insertions: diffSummary.Insertions,
			Deletions:  diffSummary.Deletions,
		},
		Duration: int(time.Since(startTime).Seconds()),
	})
	h.jobManager.UpdateStatus(job.ID, jobs.StatusCompleted)
}

func (h *Handlers) failJob(job *jobs.Job, logData *logging.RunLogData, errorMsg, errorCode string) {
	logData.Status = "failed"
	logData.Error = errorMsg
	logData.ErrorCode = errorCode
	h.jobManager.SetJobError(job.ID, errorMsg, errorCode)
}

func (h *Handlers) generateCommitMessage(changedFiles []string, instruction string) string {
	// Create summary from changed files
	var summary string
	if len(changedFiles) <= 3 {
		summary = strings.Join(changedFiles, ", ")
	} else {
		summary = fmt.Sprintf("%s and %d more files", strings.Join(changedFiles[:2], ", "), len(changedFiles)-2)
	}

	// Truncate instruction if too long
	inst := instruction
	if len(inst) > 100 {
		inst = inst[:97] + "..."
	}

	return fmt.Sprintf("%s (%s)\n\nInstruction: %s", summarizeAction(instruction), summary, inst)
}

func summarizeAction(instruction string) string {
	lower := strings.ToLower(instruction)

	if strings.Contains(lower, "add") {
		return "Add feature"
	}
	if strings.Contains(lower, "fix") {
		return "Fix issue"
	}
	if strings.Contains(lower, "update") {
		return "Update"
	}
	if strings.Contains(lower, "remove") || strings.Contains(lower, "delete") {
		return "Remove"
	}
	if strings.Contains(lower, "refactor") {
		return "Refactor"
	}

	// Default: use first few words
	words := strings.Fields(instruction)
	if len(words) > 4 {
		return strings.Join(words[:4], " ")
	}
	return instruction
}

func (h *Handlers) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (h *Handlers) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]string{"error": message})
}
