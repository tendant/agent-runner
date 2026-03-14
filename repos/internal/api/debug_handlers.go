package api

import (
	"net/http"
	"strconv"
)

// HandleDebugSchedules returns the current state of workflow schedules and recent runs.
func (h *Handlers) HandleDebugSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if h.runnerDB == nil {
		h.writeJSON(w, http.StatusOK, map[string]interface{}{
			"error":     "runner not enabled",
			"hint":      "Set RUNNER_SCHEDULER_ENABLED=true and RUNNER_DATABASE_URL to enable the runner",
			"schedules": []interface{}{},
			"runs":      []interface{}{},
		})
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	schedules, schedErr := h.runnerDB.QuerySchedules()
	runs, runsErr := h.runnerDB.QueryRuns(limit)

	result := map[string]interface{}{
		"schedules": schedules,
		"runs":      runs,
	}
	if schedErr != nil {
		result["schedules_error"] = schedErr.Error()
	}
	if runsErr != nil {
		result["runs_error"] = runsErr.Error()
	}

	h.writeJSON(w, http.StatusOK, result)
}
