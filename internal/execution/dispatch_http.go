package execution

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// JobPathPrefix is where runners on other backends reach the dispatcher
// (JOBS_DESIGN.md §7). Its routes take the session's bearer token, not the
// API key.
const JobPathPrefix = "/internal/sessions/"

// JobHandler serves GET {id}/spec, POST {id}/report, POST {id}/result and
// GET {id}/control?wait=30s under JobPathPrefix.
func (h *Engine) JobHandler() http.Handler {
	return http.HandlerFunc(h.serveJob)
}

var jobOps = map[string]string{"spec": http.MethodGet, "control": http.MethodGet, "report": http.MethodPost, "result": http.MethodPost}

func (h *Engine) serveJob(w http.ResponseWriter, r *http.Request) {
	id, op, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, JobPathPrefix), "/")
	method, known := jobOps[op]
	if !ok || id == "" || !known {
		writeJobJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if r.Method != method {
		writeJobJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	switch op {
	case "spec":
		spec, err := h.JobSpec(id, token)
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeJobJSON(w, http.StatusOK, spec)
	case "report":
		var rep JobReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			writeJobJSON(w, http.StatusBadRequest, map[string]string{"error": "bad report: " + err.Error()})
			return
		}
		if err := h.JobReport(id, token, &rep); err != nil {
			writeJobError(w, err)
			return
		}
		writeJobJSON(w, http.StatusOK, map[string]any{"seq": rep.Seq})
	case "result":
		// A result carries the run's _send/ files: give its upload longer
		// than the server's read timeout.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
		var res RunResultWire
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			writeJobJSON(w, http.StatusBadRequest, map[string]string{"error": "bad result: " + err.Error()})
			return
		}
		// The runner sends its session span's context, so finalization
		// here joins the run's trace.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		dup, err := h.JobResult(ctx, id, token, &res)
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeJobJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": dup})
	case "control":
		wait := 30 * time.Second
		if v := r.URL.Query().Get("wait"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				writeJobJSON(w, http.StatusBadRequest, map[string]string{"error": "bad wait"})
				return
			}
			wait = d
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 30*time.Second))
		c, err := h.JobControl(r.Context(), id, token, wait)
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeJobJSON(w, http.StatusOK, c)
	}
}

func writeJobJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJobError: 401 for an unknown session or a bad token, 410 for a run
// that has ended (the runner should exit), 400 for a bad request.
func writeJobError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrJobAuth):
		status = http.StatusUnauthorized
	case errors.Is(err, ErrJobEnded):
		status = http.StatusGone
	}
	writeJobJSON(w, status, map[string]string{"error": err.Error()})
}
