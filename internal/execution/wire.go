package execution

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/subagent"
)

// DecodeWire is the agent.WireDecoder for sessions this engine runs: the
// plan is a *subagent.PlanResult and the review a *subagent.ReviewResult,
// as the bots, evals and finalization expect.
func DecodeWire(field string, raw json.RawMessage) (any, error) {
	switch field {
	case "plan":
		var p subagent.PlanResult
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return &p, nil
	case "review":
		var r subagent.ReviewResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, fmt.Errorf("unknown wire field %q", field)
}

// RunResultWire is a RunResult as a runner sends it (JOBS_DESIGN.md §7.2).
// It has no redactor: a runner redacts what it reports (Redacted says so),
// since the dispatcher cannot rebuild the run's.
type RunResultWire struct {
	SessionID      string                     `json:"session_id"`
	Source         string                     `json:"source"`
	Message        string                     `json:"message"`
	Author         string                     `json:"author,omitempty"`
	StartTime      time.Time                  `json:"start_time"`
	Eval           bool                       `json:"eval,omitempty"`
	Snapshot       *agent.SessionWire         `json:"snapshot"`
	PlannerPrompt  string                     `json:"planner_prompt,omitempty"`
	PromptSections []logging.PromptSectionLog `json:"prompt_sections,omitempty"`
	PromptCuts     []string                   `json:"prompt_cuts,omitempty"`
	MemorySaved    []string                   `json:"memory_saved,omitempty"`
	MemorySkipped  []string                   `json:"memory_skipped,omitempty"`
	Egress         []logging.EgressLog        `json:"egress,omitempty"`
	Schedule       []ScheduleEntry            `json:"schedule,omitempty"`
	Proposals      []ProposalFileWire         `json:"proposals,omitempty"`
	Redacted       bool                       `json:"redacted"`
}

// ProposalFileWire is a _proposals/ file in a result.
type ProposalFileWire struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Wire returns the result as a runner sends it. What finalization would
// redact (the message, the planner prompt, the iterations' prompts and the
// error) goes through Redact first, so the secrets stay in the runner.
func (r *RunResult) Wire() (*RunResultWire, error) {
	snap, err := r.Snapshot.Wire()
	if err != nil {
		return nil, err
	}
	redact := r.Redact
	if redact == nil {
		redact = func(s string) string { return s }
	}
	for i := range snap.Iterations {
		snap.Iterations[i].Prompt = redact(snap.Iterations[i].Prompt)
	}
	snap.Error = redact(snap.Error) // the daily log, pushed with memory, quotes it
	var props []ProposalFileWire
	for _, f := range r.Proposals {
		props = append(props, ProposalFileWire{Name: f.Name, Data: f.Data})
	}
	return &RunResultWire{
		SessionID:      r.SessionID,
		Source:         r.Source,
		Message:        redact(r.Message),
		Author:         r.Author,
		StartTime:      r.StartTime,
		Eval:           r.Eval,
		Snapshot:       snap,
		PlannerPrompt:  redact(r.PlannerPrompt),
		PromptSections: r.PromptSections,
		PromptCuts:     r.PromptCuts,
		MemorySaved:    r.MemorySaved,
		MemorySkipped:  r.MemorySkipped,
		Egress:         r.Egress,
		Schedule:       r.Schedule,
		Proposals:      props,
		Redacted:       true,
	}, nil
}

// RunResult turns a received result back into what finalizeSession takes.
// It is already redacted, so its Redact is nil.
func (w *RunResultWire) RunResult() (*RunResult, error) {
	if w.Snapshot == nil {
		return nil, fmt.Errorf("run result for %s has no snapshot", w.SessionID)
	}
	if w.Snapshot.ID != w.SessionID {
		return nil, fmt.Errorf("run result for %s carries session %s", w.SessionID, w.Snapshot.ID)
	}
	snap, err := agent.NewSessionFromWire(w.Snapshot, DecodeWire)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		SessionID:      w.SessionID,
		Source:         w.Source,
		Message:        w.Message,
		Author:         w.Author,
		StartTime:      w.StartTime,
		Eval:           w.Eval,
		Snapshot:       snap,
		PlannerPrompt:  w.PlannerPrompt,
		PromptSections: w.PromptSections,
		PromptCuts:     w.PromptCuts,
		MemorySaved:    w.MemorySaved,
		MemorySkipped:  w.MemorySkipped,
		Egress:         w.Egress,
		Schedule:       w.Schedule,
	}, nil
}
