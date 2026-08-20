package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/locks"
)

// LockRequest is the POST /lock request body.
//
// Name is chosen by the agent: the runner has no idea which tasks conflict, but
// the agent does. Holder defaults to the calling session's ID.
type LockRequest struct {
	Name        string `json:"name"`
	Holder      string `json:"holder,omitempty"`
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
}

// HandleLock handles POST /lock — acquire a named advisory lease.
func (h *Handlers) HandleLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req LockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	holder := h.lockHolder(r, req.Holder)
	if holder == "" {
		h.writeError(w, http.StatusBadRequest, "holder is required (send the session id as `holder` or X-Session-ID)")
		return
	}

	lease, err := h.lockManager.Acquire(
		r.Context(),
		req.Name,
		holder,
		time.Duration(req.TTLSeconds)*time.Second,
		time.Duration(req.WaitSeconds)*time.Second,
	)
	if err != nil {
		var conflict *locks.ConflictError
		if errors.As(err, &conflict) {
			h.writeJSON(w, http.StatusConflict, map[string]any{
				"error":       err.Error(),
				"name":        conflict.Held.Name,
				"held_by":     conflict.Held.Holder,
				"held_since":  conflict.Held.AcquiredAt.Format(time.RFC3339),
				"expires_at":  conflict.Held.ExpiresAt.Format(time.RFC3339),
				"retry_after": int(time.Until(conflict.Held.ExpiresAt).Seconds()),
			})
			return
		}
		if errors.Is(err, r.Context().Err()) {
			// Client hung up mid-wait; nothing useful to send.
			return
		}
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	slog.Info("lock: acquired", "name", lease.Name, "holder", lease.Holder)
	h.writeJSON(w, http.StatusOK, map[string]any{
		"name":        lease.Name,
		"holder":      lease.Holder,
		"acquired_at": lease.AcquiredAt.Format(time.RFC3339),
		"expires_at":  lease.ExpiresAt.Format(time.RFC3339),
	})
}

// HandleUnlock handles DELETE /lock/{name} — release a lease.
func (h *Handlers) HandleUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/lock/")
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "lock name is required")
		return
	}

	holder := h.lockHolder(r, r.URL.Query().Get("holder"))
	if holder == "" {
		h.writeError(w, http.StatusBadRequest, "holder is required (send the session id as ?holder= or X-Session-ID)")
		return
	}

	if err := h.lockManager.Release(name, holder); err != nil {
		h.writeError(w, http.StatusNotFound, err.Error())
		return
	}

	slog.Info("lock: released", "name", name, "holder", holder)
	h.writeJSON(w, http.StatusOK, map[string]any{"name": name, "released": true})
}

// HandleListLocks handles GET /locks — currently held leases.
func (h *Handlers) HandleListLocks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	held := h.lockManager.List()
	out := make([]map[string]any, 0, len(held))
	for _, lease := range held {
		out = append(out, map[string]any{
			"name":        lease.Name,
			"holder":      lease.Holder,
			"acquired_at": lease.AcquiredAt.Format(time.RFC3339),
			"expires_at":  lease.ExpiresAt.Format(time.RFC3339),
		})
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"locks": out})
}

// lockHolder resolves who is asking: an explicit value from the request, else
// the X-Session-ID header. The prompt hands the agent its own session id as
// {{SESSION_ID}}, so it can identify itself without the runner guessing.
func (h *Handlers) lockHolder(r *http.Request, explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Session-ID"))
}
