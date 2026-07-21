package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/execution"
	simpleworkflow "github.com/tendant/simple-workflow"
)

// ScheduleEntry is the schedule-task payload; canonical type in execution.
type ScheduleEntry = execution.ScheduleEntry

// ScheduleInfo describes a schedule; canonical type in execution.
type ScheduleInfo = execution.ScheduleInfo

// WorkflowScheduler submits schedule entries; canonical type in execution.
type WorkflowScheduler = execution.WorkflowScheduler

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
		if payload, ok := s.Payload.(map[string]any); ok {
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

	if err := h.workflowClient.SubmitSchedule(ctx, []ScheduleEntry{entry}, h.config.Scheduler.TypePrefix); err != nil {
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

	h.writeJSON(w, http.StatusOK, map[string]any{"schedules": schedules})
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

// submitScheduleEntries submits each entry via simple-workflow client.
func submitScheduleEntries(ctx context.Context, client *simpleworkflow.Client, entries []ScheduleEntry, typePrefix string) error {
	workflowType := typePrefix + "task.v1"
	slog.Info("scheduler: submitting entries", "count", len(entries), "workflow_type", workflowType)

	for i, entry := range entries {
		if entry.Message == "" {
			slog.Warn("scheduler: skipping entry with empty message", "index", i)
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
			slog.Info("scheduler: creating cron schedule", "index", i, "cron", entry.Cron, "tz", tz, "message", entry.Message)
			schedID, err := client.Schedule(workflowType, payload).
				Cron(entry.Cron).
				InTimezone(tz).
				Create(ctx)
			if err != nil {
				slog.Warn("scheduler: failed to create cron schedule", "index", i, "error", err)
				continue
			}
			slog.Info("scheduler: created cron schedule", "schedule_id", schedID, "cron", entry.Cron, "tz", tz, "message", entry.Message)

		case entry.RunAfter != "":
			// Absolute time one-shot
			t, err := time.Parse(time.RFC3339, entry.RunAfter)
			if err != nil {
				slog.Warn("scheduler: invalid run_after", "index", i, "error", err)
				continue
			}
			slog.Info("scheduler: submitting run_after entry", "index", i, "at", entry.RunAfter,
				"in", time.Until(t).Round(time.Second).String(), "message", entry.Message, "idempotency_key", entry.IdempotencyKey)
			builder := client.Submit(workflowType, payload).RunAfter(t)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				slog.Warn("scheduler: failed to submit run_after entry", "index", i, "error", err)
				continue
			}
			if runID == "" {
				slog.Info("scheduler: run_after entry skipped (idempotency conflict)", "index", i)
			} else {
				slog.Info("scheduler: submitted run_after task", "run_id", runID, "at", entry.RunAfter, "message", entry.Message)
			}

		case entry.RunInSeconds > 0:
			// Relative delay one-shot
			runAt := time.Now().Add(time.Duration(entry.RunInSeconds) * time.Second)
			slog.Info("scheduler: submitting delayed entry", "index", i, "in_seconds", entry.RunInSeconds,
				"at", runAt.Format(time.RFC3339), "message", entry.Message, "idempotency_key", entry.IdempotencyKey)
			builder := client.Submit(workflowType, payload).RunIn(time.Duration(entry.RunInSeconds) * time.Second)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				slog.Warn("scheduler: failed to submit delayed entry", "index", i, "error", err)
				continue
			}
			if runID == "" {
				slog.Info("scheduler: delayed entry skipped (idempotency conflict)", "index", i)
			} else {
				slog.Info("scheduler: submitted delayed task", "run_id", runID, "in_seconds", entry.RunInSeconds, "message", entry.Message)
			}

		default:
			slog.Warn("scheduler: skipping entry with no scheduling mode (need run_after, run_in_seconds, or cron)", "index", i)
		}
	}

	slog.Info("scheduler: finished submitting entries", "count", len(entries))
	return nil
}
