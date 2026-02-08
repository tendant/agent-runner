package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// AgentRequest represents the POST /agent request body
type AgentRequest struct {
	Project             string   `json:"project"`
	PromptFile          string   `json:"prompt_file"`
	Paths               []string `json:"paths"`
	MaxIterations       int      `json:"max_iterations,omitempty"`
	MaxTotalSeconds     int      `json:"max_total_seconds,omitempty"`
	Author              string   `json:"author,omitempty"`
	CommitMessagePrefix string   `json:"commit_message_prefix,omitempty"`
}

// HandleStartAgent handles POST /agent — start a new agent session
func (h *Handlers) HandleStartAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req AgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Project == "" {
		h.writeError(w, http.StatusBadRequest, "project is required")
		return
	}
	if req.PromptFile == "" {
		h.writeError(w, http.StatusBadRequest, "prompt_file is required")
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

	// Apply defaults
	author := req.Author
	if author == "" {
		author = "claude-agent"
	}
	commitPrefix := req.CommitMessagePrefix
	if commitPrefix == "" {
		commitPrefix = "[agent]"
	}
	maxIter := req.MaxIterations
	if maxIter <= 0 {
		maxIter = h.config.Agent.MaxIterations
	}
	maxSeconds := req.MaxTotalSeconds
	if maxSeconds <= 0 {
		maxSeconds = h.config.Agent.MaxTotalSeconds
	}

	// Create session (acquires project lock)
	session, err := h.agentManager.CreateSession(
		req.Project, req.PromptFile, req.Paths,
		author, commitPrefix, maxIter, maxSeconds,
	)
	if err != nil {
		if strings.Contains(err.Error(), "locked") {
			h.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Capture for response before goroutine
	sessionID := session.ID

	// Start agent loop in background
	go h.executeAgent(session, projectPath)

	h.writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id": sessionID,
		"status":     "running",
		"project":    req.Project,
	})
}

// HandleGetAgent handles GET /agent/{id} — poll agent session status
func (h *Handlers) HandleGetAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	sessionID := strings.TrimPrefix(r.URL.Path, "/agent/")
	// Strip trailing /stop if present (routed separately)
	sessionID = strings.TrimSuffix(sessionID, "/stop")
	if sessionID == "" {
		h.writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	session, exists := h.agentManager.GetSession(sessionID)
	if !exists {
		h.writeError(w, http.StatusNotFound, "session not found")
		return
	}

	h.writeJSON(w, http.StatusOK, session.ToResponse())
}

// HandleStopAgent handles POST /agent/{id}/stop — graceful stop
func (h *Handlers) HandleStopAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Extract session ID: path is /agent/{id}/stop
	path := strings.TrimPrefix(r.URL.Path, "/agent/")
	sessionID := strings.TrimSuffix(path, "/stop")
	if sessionID == "" {
		h.writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	if err := h.agentManager.StopSession(sessionID); err != nil {
		h.writeError(w, http.StatusNotFound, err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"status":     "stopping",
	})
}
