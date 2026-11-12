package execution

import (
	"context"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/proposals"
)

// RunResult is what a finished run hands to finalization (JOBS_DESIGN.md §4,
// §7.2): the session's last state and what the audit and daily logs need
// that the session itself does not carry. Everything in it but Redact is
// plain data, so a job can send it to the dispatcher.
type RunResult struct {
	SessionID string
	Source    string // "api" when the session had none
	Message   string
	Author    string
	StartTime time.Time
	Eval      bool // an eval replay: no daily log, curation or memory push

	// Snapshot is the session when the run ended, before its memory merge.
	Snapshot *agent.Session

	PlannerPrompt  string
	PromptSections []logging.PromptSectionLog // the composed prompt's outline
	PromptCuts     []string                   // what the memory budget cut from it
	MemorySaved    []string                   // memory files the agent's changes were saved to
	MemorySkipped  []string                   // memory changes refused ("name: reason")
	Egress         []logging.EgressLog        // the sandbox's egress, when sandboxed

	// What a runner leaves to the dispatcher (JOBS_DESIGN.md §8.4); a local
	// run submits them itself and leaves these empty.
	Schedule  []ScheduleEntry
	Proposals []proposals.File

	// Redact removes the agent's secret values from what is logged. It is
	// the run's (built from the agent's /env secrets); nil logs as is.
	// A job will instead redact before it reports.
	Redact func(string) string
}

// Reporter receives a run's result. Finish returns once the session is
// finalized; the run cleans up its workspace after it. ctx carries the
// session's span and is not cancelled with the run.
type Reporter interface {
	Finish(ctx context.Context, res *RunResult)
}

// localReporter finalizes in process: the "local" backend.
type localReporter struct{ h *Engine }

func (r localReporter) Finish(_ context.Context, res *RunResult) { r.h.finalizeSession(res) }
