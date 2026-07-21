package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/llm"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// Notifier can send messages to configured chat channels.
type Notifier interface {
	SendNotification(ctx context.Context, message string) error
}

// ScheduleEntry represents a single scheduled task written by an agent.
type ScheduleEntry struct {
	Message        string `json:"message"`
	RunAfter       string `json:"run_after,omitempty"`       // RFC3339 absolute time
	RunInSeconds   int    `json:"run_in_seconds,omitempty"`  // relative delay from now
	Cron           string `json:"cron,omitempty"`            // cron expression for recurring
	Timezone       string `json:"timezone,omitempty"`        // timezone for cron (default UTC)
	IdempotencyKey string `json:"idempotency_key,omitempty"` // dedup key for one-shot tasks
}

// WorkflowScheduler submits schedule entries via simple-workflow.
type WorkflowScheduler interface {
	SubmitSchedule(ctx context.Context, entries []ScheduleEntry, typePrefix string) error
	ListSchedules(ctx context.Context) ([]ScheduleInfo, error)
	DeleteSchedule(ctx context.Context, scheduleID string) error
}

// ScheduleInfo represents a schedule returned by the list endpoint.
type ScheduleInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Message     string `json:"message,omitempty"`
	CronExpr    string `json:"cron"`
	Timezone    string `json:"timezone"`
	NextRunAt   string `json:"next_run_at"`
	LastRunAt   string `json:"last_run_at,omitempty"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority"`
	MaxAttempts int    `json:"max_attempts"`
}

const scheduleFileName = "_schedule.json"

// Deps provides the mutable runtime dependencies the engine reads at call
// time — they are rebuilt by the api layer on /set (RefreshRuntime) or set
// after bot initialization, so the engine must not capture them at
// construction.
type Deps interface {
	Executor() executor.Executor
	PlannerClient() llm.Client
	CuratorClient() llm.Client
	Notifier() Notifier
	WorkflowClient() WorkflowScheduler
	BootstrapPaths() (systemPrompt, promptFile string)
}

// Engine runs agent sessions end to end: workspace preparation, the optional
// planner, the iteration loop, the optional reviewer, output/memory
// finalization, and result notification.
type Engine struct {
	config           *config.Config
	agentManager     *agent.Manager
	workspaceManager *executor.WorkspaceManager
	runLogger        *logging.RunLogger
	deps             Deps
}

// New creates an Engine.
func New(cfg *config.Config, mgr *agent.Manager, wm *executor.WorkspaceManager, rl *logging.RunLogger, deps Deps) *Engine {
	return &Engine{config: cfg, agentManager: mgr, workspaceManager: wm, runLogger: rl, deps: deps}
}

// collectScheduleEntries reads _schedule.json from the workspace directory.
func collectScheduleEntries(workspacePath string) ([]ScheduleEntry, error) {
	path := filepath.Join(workspacePath, scheduleFileName)
	slog.Debug("scheduler: reading schedule file", "path", path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("scheduler: no schedule file", "path", path)
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", scheduleFileName, err)
	}

	slog.Info("scheduler: found schedule file", "file", scheduleFileName, "bytes", len(data))

	var entries []ScheduleEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", scheduleFileName, err)
	}

	slog.Info("scheduler: parsed schedule entries", "count", len(entries))
	for i, e := range entries {
		slog.Debug("scheduler: schedule entry", "index", i, "message", e.Message,
			"cron", e.Cron, "run_after", e.RunAfter, "run_in_seconds", e.RunInSeconds, "idempotency_key", e.IdempotencyKey)
	}

	return entries, nil
}
