// Package task keeps the durable record of a multi-turn task: a thread's
// unit of work that can span several agent runs (turns), stopping to ask the
// user a question and resuming in the same workspace when they answer. See
// TASKS_DESIGN.md. Enabled with AGENT_TASKS_ENABLED.
package task

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status is where a task is in its lifecycle.
type Status string

const (
	StatusWorking       Status = "working"        // a turn is running
	StatusAwaitingInput Status = "awaiting_input" // the agent asked the user a question
	StatusPaused        Status = "paused"         // a turn ended without finishing (limit, failure, restart)
	StatusDone          Status = "done"
	StatusFailed        Status = "failed" // expired or unrecoverable; the record is kept
	StatusCancelled     Status = "cancelled"
)

// Finished reports whether the task can take no more turns.
func (s Status) Finished() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// Resumable reports whether the next message in the thread continues the
// task instead of starting fresh.
func (s Status) Resumable() bool {
	return s == StatusAwaitingInput || s == StatusPaused
}

// PlanStep is one step of the task's plan, as last reported.
type PlanStep struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Turn is one agent run of the task.
type Turn struct {
	SessionID  string    `json:"session_id"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	Request    string    `json:"request"`               // what the user said to start this turn
	StopReason string    `json:"stop_reason,omitempty"` // needs_input, done, failed, ...
	Summary    string    `json:"summary,omitempty"`
	Question   string    `json:"question,omitempty"`
	CostUSD    float64   `json:"cost_usd,omitempty"`
}

// Record is a task's durable state, stored as one JSON file per thread.
type Record struct {
	ID           string     `json:"id"`
	ThreadKey    string     `json:"thread_key"`
	Status       Status     `json:"status"`
	Goal         string     `json:"goal"`
	Plan         []PlanStep `json:"plan,omitempty"`
	Decisions    []string   `json:"decisions,omitempty"`
	OpenQuestion string     `json:"open_question,omitempty"`
	Turns        []Turn     `json:"turns,omitempty"`
	Workspace    string     `json:"workspace"`
	// WorkspaceRemoved is set once the sweep has finished the workspace, so
	// it is not finished twice.
	WorkspaceRemoved bool   `json:"workspace_removed,omitempty"`
	FailReason       string `json:"fail_reason,omitempty"`
	// PauseReason says why a paused task stopped: PauseBudget, or the
	// turn's own stop reason (a limit, a failure, a restart).
	PauseReason string `json:"pause_reason,omitempty"`
	// AwaitingApproval marks an awaiting_input task whose question is
	// "proceed with this revised plan?".
	AwaitingApproval bool `json:"awaiting_approval,omitempty"`
	// Budget is what the task has used since it started or the user last
	// said to continue past a budget.
	Budget Budget `json:"budget"`
	// Queue holds new requests that arrived on a chat transport while this
	// task was unfinished; they start in order as new tasks.
	Queue     []string  `json:"queue,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PauseBudget is the PauseReason of a task stopped by a task budget.
const PauseBudget = "budget"

// MaxQueue bounds Record.Queue.
const MaxQueue = 5

// Budget is a task's usage counted against its limits.
type Budget struct {
	Turns   int     `json:"turns"`
	Seconds int     `json:"seconds"` // working time: turn run time only
	CostUSD float64 `json:"cost_usd"`
}

// Limits caps a task's Budget; zero fields are unlimited.
type Limits struct {
	MaxTurns   int
	MaxSeconds int
	MaxCostUSD float64
}

// Exceeded returns a short description of the first limit b has reached,
// or "" while within limits.
func (l Limits) Exceeded(b Budget) string {
	switch {
	case l.MaxTurns > 0 && b.Turns >= l.MaxTurns:
		return fmt.Sprintf("%d turns", b.Turns)
	case l.MaxSeconds > 0 && b.Seconds >= l.MaxSeconds:
		return fmt.Sprintf("%s of work", (time.Duration(b.Seconds) * time.Second).String())
	case l.MaxCostUSD > 0 && b.CostUSD >= l.MaxCostUSD:
		return fmt.Sprintf("$%.2f spent", b.CostUSD)
	}
	return ""
}

// Checklist renders the plan with each step's status, one per line; "" when
// there is no plan.
func (r *Record) Checklist() string {
	var sb strings.Builder
	for _, st := range r.Plan {
		mark := "⬜"
		if st.Done {
			mark = "✅"
		}
		fmt.Fprintf(&sb, "%s %s. %s\n", mark, st.ID, st.Text)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// New starts a record for a thread's first turn.
func New(threadKey, goal, workspace string) *Record {
	now := time.Now()
	return &Record{
		ID:        "task-" + uuid.NewString(),
		ThreadKey: threadKey,
		Status:    StatusWorking,
		Goal:      goal,
		Workspace: workspace,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// LastTurn returns the most recent turn, or nil.
func (r *Record) LastTurn() *Turn {
	if len(r.Turns) == 0 {
		return nil
	}
	return &r.Turns[len(r.Turns)-1]
}

// AddDecisions appends decisions not already recorded.
func (r *Record) AddDecisions(ds []string) {
	seen := make(map[string]bool, len(r.Decisions))
	for _, d := range r.Decisions {
		seen[d] = true
	}
	for _, d := range ds {
		d = strings.TrimSpace(d)
		if d != "" && !seen[d] {
			r.Decisions = append(r.Decisions, d)
			seen[d] = true
		}
	}
}

// Store keeps task records on disk, one file per thread key. Safe for
// concurrent use; records returned by Get are copies.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore returns a store rooted at dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("task store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// FileKey maps a thread key to a filesystem-safe name.
func FileKey(threadKey string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, threadKey)
}

func (s *Store) path(threadKey string) string {
	return filepath.Join(s.dir, FileKey(threadKey)+".json")
}

// Get returns the thread's task record, if any.
func (s *Store) Get(threadKey string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(threadKey)
}

func (s *Store) getLocked(threadKey string) (*Record, bool) {
	data, err := os.ReadFile(s.path(threadKey))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("task store: read failed", "thread", threadKey, "error", err)
		}
		return nil, false
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		slog.Warn("task store: corrupt record", "thread", threadKey, "error", err)
		return nil, false
	}
	return &rec, true
}

// Save writes the record atomically (write-then-rename).
func (s *Store) Save(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(rec)
}

func (s *Store) saveLocked(rec *Record) error {
	rec.UpdatedAt = time.Now()
	return s.writeRaw(rec)
}

// writeRaw writes rec as-is. Callers hold s.mu.
func (s *Store) writeRaw(rec *Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "task-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), s.path(rec.ThreadKey)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Update loads the thread's record, applies fn and saves it, atomically with
// respect to other store calls. It returns false when there is no record.
func (s *Store) Update(threadKey string, fn func(*Record)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.getLocked(threadKey)
	if !ok {
		return false, nil
	}
	fn(rec)
	return true, s.saveLocked(rec)
}

// Delete removes the thread's record.
func (s *Store) Delete(threadKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(threadKey)); err != nil && !os.IsNotExist(err) {
		slog.Warn("task store: delete failed", "thread", threadKey, "error", err)
	}
}

// List returns all records, oldest update first.
func (s *Store) List() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(data, &rec) == nil && rec.ThreadKey != "" {
			out = append(out, &rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out
}

// SweepPolicy says when a task's workspace is released.
type SweepPolicy struct {
	Retention time.Duration // after done / failed / cancelled
	IdleTTL   time.Duration // in awaiting_input / paused; the task then fails as expired
	// Finish releases a workspace (cache repos back, delete the directory).
	Finish func(workspace string)
}

// Sweep applies the policy once. Finished tasks past retention lose their
// workspace and their record; idle tasks past the TTL are failed as expired
// and lose their workspace, keeping the record until retention passes too.
// A working task is never touched.
func (s *Store) Sweep(now time.Time, p SweepPolicy) {
	for _, rec := range s.List() {
		idle := now.Sub(rec.UpdatedAt)
		switch {
		case rec.Status.Finished():
			if idle < p.Retention {
				continue
			}
			if !rec.WorkspaceRemoved && p.Finish != nil {
				p.Finish(rec.Workspace)
			}
			s.Delete(rec.ThreadKey)
			slog.Info("task: record expired", "task_id", rec.ID, "thread", rec.ThreadKey, "status", rec.Status)
		case rec.Status.Resumable():
			if idle < p.IdleTTL {
				continue
			}
			if p.Finish != nil {
				p.Finish(rec.Workspace)
			}
			_, _ = s.Update(rec.ThreadKey, func(r *Record) {
				if !r.Status.Resumable() {
					return // it moved on while we swept
				}
				r.Status = StatusFailed
				r.FailReason = "expired"
				r.OpenQuestion = ""
				r.WorkspaceRemoved = true
			})
			slog.Info("task: expired after idling", "task_id", rec.ID, "thread", rec.ThreadKey)
		}
	}
}

// ContextBlock renders the task for the next turn's prompt: the goal, plan
// progress, decisions, earlier turns and the user's reply (TASKS_DESIGN.md §7).
//
// feedback marks the message as feedback on the task (the plan may have been
// revised for it) rather than an answer to the agent's question.
func ContextBlock(rec *Record, reply string, feedback bool) string {
	var sb strings.Builder
	sb.WriteString("## Task\n\n")
	sb.WriteString("You are continuing a task you started in an earlier turn. Your workspace is as you left it.\n\n")
	fmt.Fprintf(&sb, "Goal: %s\n", strings.TrimSpace(rec.Goal))
	if len(rec.Plan) > 0 {
		sb.WriteString("Plan:\n")
		for _, st := range rec.Plan {
			mark := "todo"
			if st.Done {
				mark = "done"
			}
			fmt.Fprintf(&sb, "  %s [%s] %s\n", st.ID, mark, st.Text)
		}
	}
	if len(rec.Decisions) > 0 {
		sb.WriteString("Decisions so far:\n")
		for _, d := range rec.Decisions {
			fmt.Fprintf(&sb, "  - %s\n", d)
		}
	}
	if len(rec.Turns) > 0 {
		sb.WriteString("Previous turns:\n")
		for i, t := range rec.Turns {
			fmt.Fprintf(&sb, "  turn %d — ", i+1)
			if t.Summary != "" {
				sb.WriteString(t.Summary)
			} else {
				fmt.Fprintf(&sb, "(%s)", orDefault(t.StopReason, "no summary"))
			}
			if t.Question != "" {
				fmt.Fprintf(&sb, " (asked: %q)", t.Question)
			}
			sb.WriteString("\n")
		}
	}
	if reply = strings.TrimSpace(reply); reply != "" {
		label := "User's reply"
		if feedback {
			label = "User's feedback on the task (address it)"
		}
		fmt.Fprintf(&sb, "\n%s: %s\n", label, reply)
	}
	return sb.String()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
