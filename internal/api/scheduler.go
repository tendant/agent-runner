package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// WorkflowScheduler submits schedule entries via simple-workflow.
type WorkflowScheduler interface {
	SubmitSchedule(ctx context.Context, entries []ScheduleEntry, typePrefix string) error
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

const scheduleFileName = "_schedule.json"

// collectScheduleEntries reads _schedule.json from the repos directory.
func collectScheduleEntries(reposPath string) ([]ScheduleEntry, error) {
	path := filepath.Join(reposPath, scheduleFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", scheduleFileName, err)
	}

	var entries []ScheduleEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", scheduleFileName, err)
	}

	return entries, nil
}

// submitScheduleEntries submits each entry via simple-workflow client.
func submitScheduleEntries(ctx context.Context, client *simpleworkflow.Client, entries []ScheduleEntry, typePrefix string) error {
	workflowType := typePrefix + "task.v1"

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
			builder := client.Submit(workflowType, payload).RunAfter(t)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				log.Printf("scheduler: failed to submit run_after entry %d: %v", i, err)
				continue
			}
			log.Printf("scheduler: submitted run_after task %s (at=%s) for: %s", runID, entry.RunAfter, entry.Message)

		case entry.RunInSeconds > 0:
			// Relative delay one-shot
			builder := client.Submit(workflowType, payload).RunIn(time.Duration(entry.RunInSeconds) * time.Second)
			if entry.IdempotencyKey != "" {
				builder = builder.WithIdempotency(entry.IdempotencyKey)
			}
			runID, err := builder.Execute(ctx)
			if err != nil {
				log.Printf("scheduler: failed to submit run_in_seconds entry %d: %v", i, err)
				continue
			}
			log.Printf("scheduler: submitted delayed task %s (in=%ds) for: %s", runID, entry.RunInSeconds, entry.Message)

		default:
			log.Printf("scheduler: skipping entry %d: no scheduling mode set (need run_after, run_in_seconds, or cron)", i)
		}
	}

	return nil
}
