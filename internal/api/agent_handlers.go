package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// AgentRequest represents the POST /agent request body
type AgentRequest struct {
	Message string `json:"message"`
	Project string `json:"project"` // Optional: explicit project override
}

var projectTagRe = regexp.MustCompile(`@([\w.\-]+)`)

// parseProjectTag extracts @project-name from message text.
// Returns the project name and the message with the tag stripped.
// If no tag found, returns empty project and original message.
func parseProjectTag(message string) (string, string) {
	match := projectTagRe.FindStringSubmatchIndex(message)
	if match == nil {
		return "", message
	}
	project := message[match[2]:match[3]]
	before := message[:match[0]]
	after := message[match[1]:]
	// Collapse whitespace at the join point
	cleaned := strings.TrimSpace(before) + " " + strings.TrimSpace(after)
	cleaned = strings.TrimSpace(cleaned)
	return project, cleaned
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

	if req.Message == "" {
		h.writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Resolve project: explicit field > @project tag > default
	message := req.Message
	project := req.Project
	if project == "" {
		tagProject, cleanMessage := parseProjectTag(message)
		if tagProject != "" {
			project = tagProject
			message = cleanMessage
		}
	}
	if project == "" {
		project = h.config.Agent.DefaultProject
	}
	if project == "" {
		h.writeError(w, http.StatusBadRequest, "no project specified: use 'project' field, @project-name in message, or set AGENT_DEFAULT_PROJECT")
		return
	}

	paths := h.config.Agent.Paths
	if len(paths) == 0 {
		h.writeError(w, http.StatusInternalServerError, "agent not configured: AGENT_PATHS is required")
		return
	}

	// Check if project is allowed
	if !h.config.IsProjectAllowed(project) {
		h.writeError(w, http.StatusBadRequest, "project not in allowed_projects")
		return
	}

	author := h.config.Agent.Author
	commitPrefix := h.config.Agent.CommitPrefix
	maxIter := h.config.Agent.MaxIterations
	maxSeconds := h.config.Agent.MaxTotalSeconds

	// Create session (acquires project lock)
	session, err := h.agentManager.CreateSession(
		project, message, paths,
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
	go h.executeAgent(session)

	h.writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id": sessionID,
		"status":     "running",
		"project":    project,
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
