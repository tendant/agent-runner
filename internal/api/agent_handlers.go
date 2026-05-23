package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
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

	// /auth needs a streaming session so the URL reaches the caller via SSE.
	// Detect it before the synchronous commander check.
	lower := strings.ToLower(strings.TrimSpace(req.Message))
	if (lower == "/auth" || strings.HasPrefix(lower, "/auth ")) && lower != "/auth cancel" {
		if h.commander != nil {
			h.handleAuthStream(w, r, req.Message)
			return
		}
	}

	// Handle configuration commands synchronously — return reply directly so the
	// client doesn't need to poll and doesn't see spurious "iteration 1" output.
	if h.commander != nil {
		if reply, sessionID, ok := h.commander.Handle(req.Message, nil); ok {
			if sessionID != "" {
				h.writeJSON(w, http.StatusAccepted, map[string]any{
					"session_id": sessionID,
					"status":     "queued",
				})
			} else {
				h.writeJSON(w, http.StatusOK, map[string]any{"reply": reply})
			}
			return
		}
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

// handleAuthStream runs /auth synchronously, streaming output as SSE directly
// on the POST /agent response — no session required.
func (h *Handlers) handleAuthStream(w http.ResponseWriter, r *http.Request, message string) {
	arg := strings.TrimSpace(message[5:]) // strip "/auth"

	cli, valErr := h.commander.validateAuth(arg)
	if valErr != nil {
		h.writeJSON(w, http.StatusOK, map[string]any{"reply": "error: " + valErr.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Acquire auth lock so /auth cancel can stop this flow.
	ctx, cancel := context.WithCancel(r.Context())
	if err := h.commander.registerAuthCancel(cancel); err != nil {
		cancel()
		h.writeJSON(w, http.StatusOK, map[string]any{"reply": "error: " + err.Error()})
		return
	}
	defer func() {
		cancel()
		h.commander.releaseAuthCancel()
	}()

	// Disable the server write deadline — SSE connections are long-lived.
	http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sendLine := func(text string) {
		b, _ := json.Marshal(map[string]string{"text": text})
		fmt.Fprintf(w, "event: output\ndata: %s\n\n", b)
		flusher.Flush()
	}

	authErr := runCLIAuthFlowCtx(ctx, cli, sendLine)

	status := "completed"
	if authErr != nil {
		status = "failed"
	}
	b, _ := json.Marshal(map[string]string{"status": status})
	fmt.Fprintf(w, "event: done\ndata: %s\n\n", b)
	flusher.Flush()
}

// HandleStreamAgent streams session events via Server-Sent Events.
// GET /agent/{id}/stream
func (h *Handlers) HandleStreamAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/agent/")
	sessionID := strings.TrimSuffix(path, "/stream")
	if sessionID == "" {
		h.writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}

	liveSession, exists := h.agentManager.GetSessionDirect(sessionID)
	if !exists {
		h.writeError(w, http.StatusNotFound, "session not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Disable the server write deadline — SSE connections are long-lived.
	http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sendEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	updates := liveSession.Subscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	lastSentCount := 0
	lastLogCount := 0

	emit := func() (done bool) {
		snap := liveSession.Snapshot()

		// emit new log lines (auth flow progress, etc.)
		for _, line := range snap.LogLines[lastLogCount:] {
			sendEvent("output", map[string]any{
				"session_id": snap.ID,
				"text":       line,
			})
		}
		lastLogCount = len(snap.LogLines)

		// emit any newly completed iterations
		for _, iter := range snap.Iterations[lastSentCount:] {
			sendEvent("iteration_done", map[string]any{
				"session_id":       snap.ID,
				"iteration":        iter.Iteration,
				"status":           iter.Status,
				"output":           iter.Output,
				"duration_seconds": iter.DurationSecs,
			})
		}
		lastSentCount = len(snap.Iterations)

		// if an iteration is in progress, emit iteration_start
		if snap.CurrentIteration > lastSentCount {
			sendEvent("iteration_start", map[string]any{
				"session_id": snap.ID,
				"iteration":  snap.CurrentIteration,
			})
		}

		if snap.Status == "completed" || snap.Status == "failed" {
			output := ""
			for i := len(snap.Iterations) - 1; i >= 0; i-- {
				if snap.Iterations[i].Output != "" {
					output = snap.Iterations[i].Output
					break
				}
			}
			sendEvent("done", map[string]any{
				"session_id":            snap.ID,
				"status":                snap.Status,
				"successful_iterations": snap.SuccessfulIterations,
				"elapsed_seconds":       snap.ElapsedSeconds,
				"error":                 snap.Error,
				"output":                output,
			})
			return true
		}
		return false
	}

	// emit initial state (catches sessions already in progress or done)
	if emit() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-updates:
			if emit() {
				return
			}
		}
	}
}

// HandleListSessions handles GET /sessions — list recent sessions newest-first.
func (h *Handlers) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	sessions := h.agentManager.ListSessions(50)
	result := make([]any, len(sessions))
	for i, s := range sessions {
		result[i] = s.ToResponse()
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"sessions": result})
}
