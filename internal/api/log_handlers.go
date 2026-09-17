package api

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/agent-runner/agent-runner/internal/logging"
)

// HandleListLogs handles GET /logs?limit=N — the most recent agent audit
// logs, newest first. Unlike GET /sessions this survives a restart: it reads
// the markdown files the engine writes on every terminal status.
func (h *Handlers) HandleListLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > 500 {
			h.writeError(w, http.StatusBadRequest, "limit must be 1-500")
			return
		}
		limit = n
	}
	summaries, err := h.runLogger.ListRecentAgentLogs(limit, "")
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, logSummaryResponse(s))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// HandleGetLog handles GET /logs/{session_id} — the full audit log for one
// session as markdown: every iteration's prompt, output, error, cost, plus
// planner/review JSON. The id may be a unique prefix.
func (h *Handlers) HandleGetLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/logs/")
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		h.writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	matches, err := h.runLogger.ListRecentAgentLogs(0, id)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case len(matches) == 0:
		h.writeError(w, http.StatusNotFound, "no audit log for session "+id)
		return
	case len(matches) > 1 && matches[0].SessionID != id:
		h.writeError(w, http.StatusConflict, "session_id prefix is ambiguous")
		return
	}
	data, err := os.ReadFile(matches[0].Path)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Agent-Session-ID", matches[0].SessionID)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func logSummaryResponse(s logging.AgentLogSummary) map[string]any {
	m := map[string]any{
		"session_id": s.SessionID,
		"status":     s.Status,
		"message":    s.Message,
		"timestamp":  s.Timestamp,
		"path":       s.Path,
	}
	if s.Duration != "" {
		m["duration"] = s.Duration
	}
	if s.Iterations != "" {
		m["iterations"] = s.Iterations
	}
	if s.Cost != "" {
		m["cost"] = s.Cost
	}
	if s.Error != "" {
		m["error"] = s.Error
	}
	return m
}
