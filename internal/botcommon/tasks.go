package botcommon

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/task"
)

// TaskStarter is implemented by an AgentStarter that can run multi-turn
// tasks (the execution engine): each turn runs in the task's persistent
// workspace. Engine.Tasks enables task mode only when the Starter has it.
type TaskStarter interface {
	StartTaskTurn(message, source, convID, taskDir string) (sessionID string, err error)
	TaskWorkspacePath(taskKey string) string
	FinishTaskWorkspace(taskDir string)
}

// taskStarter returns the starter's task support when task mode is on.
func (e *Engine) taskStarter() (TaskStarter, bool) {
	if e.Tasks == nil {
		return nil, false
	}
	ts, ok := e.Starter.(TaskStarter)
	return ts, ok
}

// ResumesTask reports whether the thread's next message continues its task:
// the agent asked a question (awaiting_input) or the last turn stopped short
// (paused). Such a message is the answer and skips the intent analyzer.
func (e *Engine) ResumesTask(id string) bool {
	if _, ok := e.taskStarter(); !ok {
		return false
	}
	rec, ok := e.Tasks.Get(id)
	return ok && rec.Status.Resumable()
}

// startTaskTurn starts the thread's next task turn: it resumes a resumable
// task with its context block, or starts a new task (releasing a finished
// predecessor's workspace). message is the prompt for a new task; request
// is the user's latest message.
func (e *Engine) startTaskTurn(ts TaskStarter, id, message, request string) (string, error) {
	rec, ok := e.Tasks.Get(id)
	if ok && rec.Status.Resumable() {
		message = task.ContextBlock(rec, request)
	} else {
		if ok && !rec.WorkspaceRemoved {
			ts.FinishTaskWorkspace(rec.Workspace)
		}
		rec = task.New(id, request, "")
		rec.Workspace = ts.TaskWorkspacePath(rec.ID)
	}

	sessionID, err := ts.StartTaskTurn(message, e.Source, id, rec.Workspace)
	if err != nil {
		return "", err
	}
	rec.Status = task.StatusWorking
	rec.OpenQuestion = ""
	rec.Turns = append(rec.Turns, task.Turn{SessionID: sessionID, StartedAt: time.Now(), Request: request})
	if err := e.Tasks.Save(rec); err != nil {
		slog.Warn(e.Label+": could not save task record", "id", id, "task_id", rec.ID, "error", err)
	}
	slog.Info(e.Label+": task turn started", "id", id, "task_id", rec.ID, "turn", len(rec.Turns), "session_id", sessionID)
	return sessionID, nil
}

// finishTaskTurn folds a finished turn into the thread's task record and
// returns the question the agent asked, if the task now waits on the user.
func (e *Engine) finishTaskTurn(id, sessionID string, session *agent.Session, sessionOk bool) (question string) {
	ts, ok := e.taskStarter()
	if !ok {
		return ""
	}
	var release string
	found, err := e.Tasks.Update(id, func(rec *task.Record) {
		turn := rec.LastTurn()
		if turn == nil || turn.SessionID != sessionID {
			return // not this task's turn (e.g. started before tasks were enabled)
		}
		turn.EndedAt = time.Now()
		if !sessionOk {
			turn.StopReason = "lost"
			if rec.Status == task.StatusWorking {
				rec.Status = task.StatusPaused
			}
			return
		}
		turn.Summary = session.TurnSummary
		turn.CostUSD = session.TotalCostUSD
		rec.AddDecisions(session.TurnDecisions)
		if plan, ok := session.PlanJSON.(*subagent.PlanResult); ok && plan != nil {
			done := make(map[string]bool, len(session.CompletedSteps))
			for _, s := range session.CompletedSteps {
				done[s] = true
			}
			rec.Plan = rec.Plan[:0]
			for _, st := range plan.Steps {
				rec.Plan = append(rec.Plan, task.PlanStep{ID: st.ID, Text: st.Description, Done: st.Done || done[st.ID]})
			}
		}

		if rec.Status == task.StatusCancelled {
			// Cancelled while the turn ran: release the workspace now.
			turn.StopReason = "cancelled"
			release = rec.Workspace
			rec.WorkspaceRemoved = true
			return
		}
		completed := session.Status == agent.SessionStatusCompleted
		switch {
		case completed && session.TurnStatus == subagent.TurnNeedsInput && session.TurnQuestion != "":
			rec.Status = task.StatusAwaitingInput
			rec.OpenQuestion = session.TurnQuestion
			turn.StopReason = "needs_input"
			turn.Question = session.TurnQuestion
			question = session.TurnQuestion
		case completed:
			rec.Status = task.StatusDone
			turn.StopReason = "done"
		default:
			// A limit, failure or stop: the workspace is kept, so the next
			// message in the thread continues from here.
			rec.Status = task.StatusPaused
			turn.StopReason = orDefault(session.Error, string(session.Status))
		}
	})
	if err != nil {
		slog.Warn(e.Label+": could not update task record", "id", id, "error", err)
	}
	if found && release != "" {
		ts.FinishTaskWorkspace(release)
	}
	return question
}

// ResetThread cancels the thread's task (if any) and completes the thread —
// the /cancel path. An idle task's workspace is released now; one with a
// turn still running is released when that turn ends.
func (e *Engine) ResetThread(id string) {
	if ts, ok := e.taskStarter(); ok {
		var release string
		_, _ = e.Tasks.Update(id, func(rec *task.Record) {
			if rec.Status.Finished() {
				return
			}
			running := rec.Status == task.StatusWorking
			rec.Status = task.StatusCancelled
			rec.OpenQuestion = ""
			if !running && !rec.WorkspaceRemoved {
				release = rec.Workspace
				rec.WorkspaceRemoved = true
			}
		})
		if release != "" {
			ts.FinishTaskWorkspace(release)
		}
	}
	e.ThreadManager.Complete(id)
}

// FormatTurnQuestion renders the question a task turn ended on.
func FormatTurnQuestion(question string) string {
	return "❓ " + strings.TrimSpace(question) + "\n\nReply to continue."
}

// RunTaskSweeper releases task workspaces past their retention or idle TTL
// (see task.Store.Sweep) until ctx is done.
func RunTaskSweeper(ctx context.Context, store *task.Store, interval time.Duration, p task.SweepPolicy) {
	store.Sweep(time.Now(), p)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			store.Sweep(now, p)
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
