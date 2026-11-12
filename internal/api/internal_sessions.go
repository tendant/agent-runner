package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/execution"
)

// internalSessionsPrefix is where runners on other backends reach the
// dispatcher (JOBS_DESIGN.md §7). These routes take the session's bearer
// token instead of the API key.
const internalSessionsPrefix = "/internal/sessions/"

// HandleInternalSession serves GET {id}/spec, POST {id}/report,
// POST {id}/result and GET {id}/control?wait=30s.
func (h *Handlers) HandleInternalSession(w http.ResponseWriter, r *http.Request) {
	id, op, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, internalSessionsPrefix), "/")
	if !ok || id == "" || strings.Contains(op, "/") {
		h.writeError(w, http.StatusNotFound, "not found")
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	eng := h.execEngine

	want := map[string]string{"spec": http.MethodGet, "control": http.MethodGet, "report": http.MethodPost, "result": http.MethodPost}
	method, known := want[op]
	if !known {
		h.writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != method {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	switch op {
	case "spec":
		spec, err := eng.JobSpec(id, token)
		if err != nil {
			h.writeJobError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, spec)
	case "report":
		var rep execution.JobReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			h.writeError(w, http.StatusBadRequest, "bad report: "+err.Error())
			return
		}
		if err := eng.JobReport(id, token, &rep); err != nil {
			h.writeJobError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"seq": rep.Seq})
	case "result":
		// A result carries the run's _send/ files: give its upload longer
		// than the server's read timeout.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
		var res execution.RunResultWire
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			h.writeError(w, http.StatusBadRequest, "bad result: "+err.Error())
			return
		}
		dup, err := eng.JobResult(id, token, &res)
		if err != nil {
			h.writeJobError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": dup})
	case "control":
		wait := 30 * time.Second
		if v := r.URL.Query().Get("wait"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				h.writeError(w, http.StatusBadRequest, "bad wait")
				return
			}
			wait = d
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 30*time.Second))
		c, err := eng.JobControl(r.Context(), id, token, wait)
		if err != nil {
			h.writeJobError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, c)
	}
}

// writeJobError: 401 for an unknown session or a bad token, 410 for a run
// that has ended (the runner should exit), 400 for a bad request.
func (h *Handlers) writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, execution.ErrJobAuth):
		h.writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, execution.ErrJobEnded):
		h.writeError(w, http.StatusGone, err.Error())
	default:
		h.writeError(w, http.StatusBadRequest, err.Error())
	}
}
