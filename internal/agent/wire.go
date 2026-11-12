package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// SessionWire is a session's whole state as JSON, for a session that runs in
// another process than the one that owns it (JOBS_DESIGN.md §7.4): the
// runner sends it in its reports and the dispatcher applies it. Session hides
// much of its state from its API JSON (json:"-"); this type hides nothing.
// TestSessionWireCoversSession fails when a Session field has no counterpart.
type SessionWire struct {
	ID                   string           `json:"session_id"`
	Source               string           `json:"source,omitempty"`
	ConvID               string           `json:"conv_id,omitempty"`
	Message              string           `json:"message"`
	Messages             []Message        `json:"messages,omitempty"`
	Paths                []string         `json:"paths,omitempty"`
	Author               string           `json:"author,omitempty"`
	CommitMessagePrefix  string           `json:"commit_message_prefix,omitempty"`
	MaxIterations        int              `json:"max_iterations"`
	MaxTotalSeconds      int              `json:"max_total_seconds"`
	Status               SessionStatus    `json:"status"`
	CurrentIteration     int              `json:"current_iteration"`
	SuccessfulIterations int              `json:"successful_iterations"`
	Iterations           []IterationWire  `json:"iterations,omitempty"`
	QueuedAt             time.Time        `json:"queued_at,omitempty"`
	StartedAt            time.Time        `json:"started_at"`
	CompletedAt          *time.Time       `json:"completed_at,omitempty"`
	Error                string           `json:"error,omitempty"`
	Warnings             []string         `json:"warnings,omitempty"`
	Notes                []string         `json:"notes,omitempty"`
	Eval                 *EvalSpec        `json:"eval,omitempty"`
	EvalResults          []EvalResult     `json:"eval_results,omitempty"`
	Pushed               []PushedCommit   `json:"pushed,omitempty"`
	CallbackURL          string           `json:"callback_url,omitempty"`
	TraceID              string           `json:"trace_id,omitempty"`
	WorkspacePath        string           `json:"workspace_path,omitempty"`
	ElapsedSeconds       int              `json:"elapsed_seconds"`
	TotalCostUSD         float64          `json:"total_cost_usd,omitempty"`
	ConsecutiveFailures  int              `json:"consecutive_failures,omitempty"`
	OutputFiles          []OutputFileWire `json:"output_files,omitempty"`
	CompletedSteps       []string         `json:"completed_steps,omitempty"`
	PlanStepCount        int              `json:"plan_step_count,omitempty"`
	Plan                 json.RawMessage  `json:"plan,omitempty"`   // PlanJSON, decoded by a WireDecoder
	Review               json.RawMessage  `json:"review,omitempty"` // ReviewJSON, decoded by a WireDecoder
	TaskWorkspace        string           `json:"task_workspace,omitempty"`
	TaskFeedback         string           `json:"task_feedback,omitempty"`
	TurnApproval         bool             `json:"turn_approval,omitempty"`
	TurnStatus           string           `json:"turn_status,omitempty"`
	TurnQuestion         string           `json:"turn_question,omitempty"`
	TurnSummary          string           `json:"turn_summary,omitempty"`
	TurnDecisions        []string         `json:"turn_decisions,omitempty"`
	UnsavedWork          string           `json:"unsaved_work,omitempty"`
	StopRequested        bool             `json:"stop_requested,omitempty"`
	AgentEvents          []ExecEvent      `json:"agent_events,omitempty"`
	ExecEventSeq         uint64           `json:"exec_event_seq,omitempty"`
	LogLines             []string         `json:"log_lines,omitempty"`
}

// IterationWire is an IterationResult with the prompt it hides from the API.
type IterationWire struct {
	IterationResult
	Prompt string `json:"prompt,omitempty"`
}

// OutputFileWire is an OutputFile with its content.
type OutputFileWire struct {
	OutputFile
	Data []byte `json:"data,omitempty"`
}

// WireDecoder turns a session's plan or review (field "plan" or "review")
// back into the typed value PlanJSON and ReviewJSON hold. The agent package
// does not know those types; the execution package does.
type WireDecoder func(field string, raw json.RawMessage) (any, error)

// Wire returns the session's whole state. The plan and review are encoded as
// JSON; a value that cannot be is an error.
func (s *Session) Wire() (*SessionWire, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w := &SessionWire{
		ID:                   s.ID,
		Source:               s.Source,
		ConvID:               s.ConvID,
		Message:              s.Message,
		Messages:             append([]Message(nil), s.Messages...),
		Paths:                append([]string(nil), s.Paths...),
		Author:               s.Author,
		CommitMessagePrefix:  s.CommitMessagePrefix,
		MaxIterations:        s.MaxIterations,
		MaxTotalSeconds:      s.MaxTotalSeconds,
		Status:               s.Status,
		CurrentIteration:     s.CurrentIteration,
		SuccessfulIterations: s.SuccessfulIterations,
		QueuedAt:             s.QueuedAt,
		StartedAt:            s.StartedAt,
		Error:                s.Error,
		Warnings:             append([]string(nil), s.Warnings...),
		Notes:                append([]string(nil), s.Notes...),
		Eval:                 s.Eval,
		EvalResults:          append([]EvalResult(nil), s.EvalResults...),
		Pushed:               append([]PushedCommit(nil), s.Pushed...),
		CallbackURL:          s.CallbackURL,
		TraceID:              s.TraceID,
		WorkspacePath:        s.WorkspacePath,
		ElapsedSeconds:       s.ElapsedSeconds,
		TotalCostUSD:         s.TotalCostUSD,
		ConsecutiveFailures:  s.ConsecutiveFailures,
		CompletedSteps:       append([]string(nil), s.CompletedSteps...),
		PlanStepCount:        s.PlanStepCount,
		TaskWorkspace:        s.TaskWorkspace,
		TaskFeedback:         s.TaskFeedback,
		TurnApproval:         s.TurnApproval,
		TurnStatus:           s.TurnStatus,
		TurnQuestion:         s.TurnQuestion,
		TurnSummary:          s.TurnSummary,
		TurnDecisions:        append([]string(nil), s.TurnDecisions...),
		UnsavedWork:          s.UnsavedWork,
		StopRequested:        s.stopRequested,
		AgentEvents:          append([]ExecEvent(nil), s.AgentEvents...),
		ExecEventSeq:         s.execEventSeq,
		LogLines:             append([]string(nil), s.LogLines...),
	}
	if s.CompletedAt != nil {
		t := *s.CompletedAt
		w.CompletedAt = &t
	}
	for _, it := range s.Iterations {
		w.Iterations = append(w.Iterations, IterationWire{IterationResult: it, Prompt: it.Prompt})
	}
	for _, f := range s.OutputFiles {
		w.OutputFiles = append(w.OutputFiles, OutputFileWire{OutputFile: f, Data: f.Data})
	}
	var err error
	if w.Plan, err = marshalPart(s.PlanJSON); err != nil {
		return nil, fmt.Errorf("session %s plan: %w", s.ID, err)
	}
	if w.Review, err = marshalPart(s.ReviewJSON); err != nil {
		return nil, fmt.Errorf("session %s review: %w", s.ID, err)
	}
	return w, nil
}

func marshalPart(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// ApplyWire replaces the session's state with w's (w must be this session's)
// and wakes its subscribers. decode turns the plan and review back into
// their types; with nil decode they are kept as json.RawMessage. The
// session's redactor, stop channel and subscribers are its own and stay.
func (s *Session) ApplyWire(w *SessionWire, decode WireDecoder) error {
	if w.ID != s.ID {
		return fmt.Errorf("wire for session %q applied to %q", w.ID, s.ID)
	}
	plan, err := decodePart(decode, "plan", w.Plan)
	if err != nil {
		return err
	}
	review, err := decodePart(decode, "review", w.Review)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.Source = w.Source
	s.ConvID = w.ConvID
	s.Message = w.Message
	s.Messages = append([]Message(nil), w.Messages...)
	s.Paths = append([]string(nil), w.Paths...)
	s.Author = w.Author
	s.CommitMessagePrefix = w.CommitMessagePrefix
	s.MaxIterations = w.MaxIterations
	s.MaxTotalSeconds = w.MaxTotalSeconds
	s.Status = w.Status
	s.CurrentIteration = w.CurrentIteration
	s.SuccessfulIterations = w.SuccessfulIterations
	s.Iterations = nil
	for _, it := range w.Iterations {
		r := it.IterationResult
		r.Prompt = it.Prompt
		s.Iterations = append(s.Iterations, r)
	}
	s.QueuedAt = w.QueuedAt
	s.StartedAt = w.StartedAt
	s.CompletedAt = nil
	if w.CompletedAt != nil {
		t := *w.CompletedAt
		s.CompletedAt = &t
	}
	s.Error = w.Error
	s.Warnings = append([]string(nil), w.Warnings...)
	s.Notes = append([]string(nil), w.Notes...)
	s.Eval = w.Eval
	s.EvalResults = append([]EvalResult(nil), w.EvalResults...)
	s.Pushed = append([]PushedCommit(nil), w.Pushed...)
	s.CallbackURL = w.CallbackURL
	s.TraceID = w.TraceID
	s.WorkspacePath = w.WorkspacePath
	s.ElapsedSeconds = w.ElapsedSeconds
	s.TotalCostUSD = w.TotalCostUSD
	s.ConsecutiveFailures = w.ConsecutiveFailures
	s.OutputFiles = nil
	for _, f := range w.OutputFiles {
		o := f.OutputFile
		o.Data = f.Data
		s.OutputFiles = append(s.OutputFiles, o)
	}
	s.CompletedSteps = append([]string(nil), w.CompletedSteps...)
	s.PlanStepCount = w.PlanStepCount
	s.PlanJSON = plan
	s.ReviewJSON = review
	s.TaskWorkspace = w.TaskWorkspace
	s.TaskFeedback = w.TaskFeedback
	s.TurnApproval = w.TurnApproval
	s.TurnStatus = w.TurnStatus
	s.TurnQuestion = w.TurnQuestion
	s.TurnSummary = w.TurnSummary
	s.TurnDecisions = append([]string(nil), w.TurnDecisions...)
	s.UnsavedWork = w.UnsavedWork
	s.stopRequested = s.stopRequested || w.StopRequested // a stop is never taken back
	s.AgentEvents = append([]ExecEvent(nil), w.AgentEvents...)
	s.execEventSeq = w.ExecEventSeq
	s.LogLines = append([]string(nil), w.LogLines...)
	s.mu.Unlock()
	s.notifyUpdate()
	return nil
}

// NewSessionFromWire makes a session from a wire state: how a runner gets
// the session it runs (JOBS_DESIGN.md §7.1).
func NewSessionFromWire(w *SessionWire, decode WireDecoder) (*Session, error) {
	s := &Session{ID: w.ID, notify: make(chan struct{}, 1)}
	if err := s.ApplyWire(w, decode); err != nil {
		return nil, err
	}
	return s, nil
}

func decodePart(decode WireDecoder, field string, raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if decode == nil {
		return append(json.RawMessage(nil), raw...), nil
	}
	v, err := decode(field, raw)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", field, err)
	}
	return v, nil
}
