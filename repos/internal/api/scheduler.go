package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	simpleworkflow "github.com/tendant/simple-workflow"
)

// ScheduleEntry represents a single scheduled task written by an agent.
type ScheduleEntry struct {
	Message        string `json:"message"`
	RunAfter       string `json:"run_after,omitempty"`        // RFC3339 absolute time
	RunInSeconds   int    `json:"run_in_seconds,omitempty"`   // relative delay from now
	Cron           string `json:"cron,omitempty"`             // cron expression for recurring
	Timezone       string `json:"timezone,omitempty"`         // timezone for cron (default UTC)
	IdempotencyKey string `json:"idempotency_key,omitempty"` // dedup key for one-shot tasks
}

// ScheduleInfo represents a schedule returned by the list endpoint.
type ScheduleInfo struct {
	ID          string  `json:"id"`
	Type        string  `json:"type"`
	Message     string  `json:"message,omitempty"`
	CronExpr    string  `json:"cron"`
	Timezone    string  `json:"timezone"`
	NextRunAt   string  `json:"next_run_at"`
	LastRunAt   string  `json:"last_run_at,omitempty"`
	Enabled     bool    `json:"enabled"`
	Priority    int     `json:"priority"`
	MaxAttempts int     `json:"max_attempts"`
}

// WorkflowScheduler submits schedule entries via simple-workflow.
type WorkflowScheduler interface {
	SubmitSchedule(ctx context.Context, entries []ScheduleEntry, typePrefix string) error
	ListSchedules(ctx context.Context) ([]ScheduleInfo, error)
	DeleteSchedule(ctx context.Context, scheduleID string) error
}

// workflowSchedulerClient implements WorkflowScheduler using a simple-workflow Client.
type workflowSchedulerClient struct {
	client *simpleworkflow.Client
}

// NewWorkflowScheduler creates a WorkflowScheduler backed by a simple-workflow Client.
func NewWorkflowScheduler(client *simpleworkflow.Client) WorkflowScheduler {
	return &workflowSchedulerClient{client: client}
}

func (w *workflowSchedulerClient) SubmitSchedule(ctx context.Context, entries []ScheduleEntry, typePrefix string) error {
	return submitScheduleEntries(ctx, w.client, entries, typePrefix)
}

func (w *workflowSchedulerClient) ListSchedules(ctx context.Context) ([]ScheduleInfo, error) {
	schedules, err := w.client.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	var result []ScheduleInfo
	for _, s := range schedules {
		info := ScheduleInfo{
			ID:          s.ID,
			Type:        s.Type,
			CronExpr:    s.CronExpr,
			Timezone:    s.Timezone,
			NextRunAt:   s.NextRunAt.Format(time.RFC3339),
			Enabled:     s.Enabled,
			Priority:    s.Priority,
			MaxAttempts: s.MaxAttempts,
		}
		if s.LastRunAt != nil {
			info.LastRunAt = s.LastRunAt.Format(time.RFC3339)
		}
		// Extract message from payload if available
		if payload, ok := s.Payload.(map[string]interface{}); ok {
			if msg, ok := payload["message"].(string); ok {
				info.Message = msg
			}
		}
		result = append(result, info)
	}
	return result, nil
}

func (w *workflowSchedulerClient) DeleteSchedule(ctx context.Context, scheduleID string) error {
	return w.client.DeleteSchedule(ctx, scheduleID)
}

// HandleSchedule handles POST /schedule requests from agents to create scheduled tasks.
func (h *Handlers) HandleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var entry ScheduleEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	if entry.Message == "" {
		h.writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	if entry.RunAfter == "" && entry.RunInSeconds == 0 && entry.Cron == "" {
		h.writeError(w, http.StatusBadRequest, "at least one scheduling mode is required (run_after, run_in_seconds, or cron)")
		return
	}

	if h.workflowClient == nil {
		h.writeError(w, http.StatusServiceUnavailable, "runner not enabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.workflowClient.SubmitSchedule(ctx, []ScheduleEntry{entry}, h.config.Runner.TypePrefix); err != nil {
		h.writeError(w, http.StatusBadGateway, "failed to submit schedule: "+err.Error())
		return
	}

	h.writeJSON(w, http.StatusAccepted, map[string]string{"status": "scheduled"})
}

// HandleListSchedules handles GET /schedules — returns all active schedules.
func (h *Handlers) HandleListSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	if h.workflowClient == nil {
		h.writeError(w, http.StatusServiceUnavailable, "runner not enabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	schedules, err := h.workflowClient.ListSchedules(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "failed to list schedules: "+err.Error())
		return
	}

	if schedules == nil {
		schedules = []ScheduleInfo{}
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{"schedules": schedules})
}

// HandleDeleteSchedule handles DELETE /schedule/{id} — soft-deletes a schedule.
func (h *Handlers) HandleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	scheduleID := strings.TrimPrefix(r.URL.Path, "/schedule/")
	if scheduleID == "" {
		h.writeError(w, http.StatusBadRequest, "schedule id is required")
		return
	}

	if h.workflowClient == nil {
		h.writeError(w, http.StatusServiceUnavailable, "runner not enabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.workflowClient.DeleteSchedule(ctx, scheduleID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.writeError(w, http.StatusNotFound, "schedule not found")
			return
		}
		h.writeError(w, http.StatusInternalServerError, "failed to delete schedule: "+err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

const scheduleFileName = "_schedule.json"

// collectScheduleEntries reads _schedule.json from the workspace directory.
func collectScheduleEntries(workspacePath string) ([]ScheduleEntry, error) {
	path := filepath.Join(workspacePath, scheduleFileName)
	log.Printf("scheduler: reading %s", path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("scheduler: no %s found at %s", scheduleFileName, path)
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", scheduleFileName, err)
	}

	log.Printf("scheduler: found %s (%d bytes)", scheduleFileName, len(data))

	var entries []ScheduleEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", scheduleFileName, err)
	}

	log.Printf("scheduler: parsed %d entries from %s", len(entries), scheduleFileName)
	for i, e := range entries {
		log.Printf("scheduler: entry %d: message=%q cron=%q run_after=%q run_in_seconds=%d idempotency_key=%q",
			i, e.Message, e.Cron, e.RunAfter, e.RunInSeconds, e.IdempotencyKey)
	}

	return entries, nil
}

// submitScheduleEntries submits each entry via simple-workflow client.
func submitScheduleEntries(ctx context.Context, client *simpleworkflow.Client, entries []ScheduleEntry, typePrefix string) error {
	workflowType := typePrefix + "task.v1"
	log.Printf("scheduler: submitting %d entries with workflow_type=%s", len(entries), workflowType)

	for i, entry := range entries {
		if entry.Message == "" {
			log.Printf("scheduler: skipping entry %d: empty message", i)
			continue
		}

		payload := map[string]string{"message": entry.Message}

		switch {
		case entry.Cron != "":
			// Recurring schedule
			tz := entry.Timezone
			if tz == "" {
				tz = "UTC"
			}
			log.Printf("scheduler: creating cron schedule entry %d: cron=%q tz=%s message=%q", i, entry.Cron, tz, entry.Message)
			schedID, err := client.Schedule(workflowType, payload).
				Cron(entry.Cron).
				InTimezone(tz).
				Create(ctx)
			if err != nil {
				log.Printf("scheduler: failed to create cron schedule for entry %d: %v", i, err)
				continue
			}
			log.Printf("scheduler: created cron schedule %s (cron=%s tz=%s) for: %s", schedID, entry.Cron, tz, entry.Message)

		case entry.RunAfter != "":
			// Absolute time one-shot
			t, err := time.Parse(time.RFC3339, entry.RunAfter)
			if err != nil {
				log.Printf("scheduler: invalid run_after for entry %d: %v", i, err)
				continue
			}
			log.Printf("scheduler: submitting run_after entry %d: at=%s (in %s) message=%q idem_key=%q",
				i, entry.RunAfter, time.Until(t).Round(time.Second), entry.Message, entry.IdempotencyKey)
			builder := client.Submit(workflowType, payload).RunAfter(t)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				log.Printf("scheduler: failed to submit run_after entry %d: %v", i, err)
				continue
			}
			if runID == "" {
				log.Printf("scheduler: run_after entry %d: skipped (idempotency conflict, already exists)", i)
			} else {
				log.Printf("scheduler: submitted run_after task %s (at=%s) for: %s", runID, entry.RunAfter, entry.Message)
			}

		case entry.RunInSeconds > 0:
			// Relative delay one-shot
			runAt := time.Now().Add(time.Duration(entry.RunInSeconds) * time.Second)
			log.Printf("scheduler: submitting delayed entry %d: in=%ds (at ~%s) message=%q idem_key=%q",
				i, entry.RunInSeconds, runAt.Format(time.RFC3339), entry.Message, entry.IdempotencyKey)
			builder := client.Submit(workflowType, payload).RunIn(time.Duration(entry.RunInSeconds) * time.Second)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				log.Printf("scheduler: failed to submit run_in_seconds entry %d: %v", i, err)
				continue
			}
			if runID == "" {
				log.Printf("scheduler: delayed entry %d: skipped (idempotency conflict, already exists)", i)
			} else {
				log.Printf("scheduler: submitted delayed task %s (in=%ds) for: %s", runID, entry.RunInSeconds, entry.Message)
			}

		default:
			log.Printf("scheduler: skipping entry %d: no scheduling mode set (need run_after, run_in_seconds, or cron)", i)
		}
	}

	log.Printf("scheduler: finished submitting %d entries", len(entries))
	return nil
}
