package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// AgentRequest represents the POST /agent request body
type AgentRequest struct {
	Message string `json:"message"`
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

	paths := h.config.Agent.Paths
	author := h.config.Agent.Author
	commitPrefix := h.config.Agent.CommitPrefix
	maxIter := h.config.Agent.MaxIterations
	maxSeconds := h.config.Agent.MaxTotalSeconds

	// Create session
	session, err := h.agentManager.CreateSession(
		req.Message, paths,
		author, commitPrefix, maxIter, maxSeconds,
	)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	session.Source = "api"

	// Capture for response before goroutine
	sessionID := session.ID

	preview := req.Message
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	slog.Info("agent session started", "session_id", sessionID, "message_len", len(req.Message), "message", preview)

	// Enqueue agent for serial dispatch
	if err := h.agentManager.Enqueue(session, h.executeAgent); err != nil {
		h.agentManager.FailSession(sessionID, "agent queue is full")
		h.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "agent queue is full"})
		return
	}

	h.writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id": sessionID,
		"status":     "queued",
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
