package botcommon

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/task"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// TaskStarter is implemented by an AgentStarter that can run multi-turn
// tasks (the execution engine): each turn runs in the task's persistent
// workspace. Engine.Tasks enables task mode only when the Starter has it.
type TaskStarter interface {
	StartTaskTurn(turn agent.TaskTurn) (sessionID string, err error)
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
// (paused).
func (e *Engine) ResumesTask(id string) bool {
	if _, ok := e.taskStarter(); !ok {
		return false
	}
	rec, ok := e.Tasks.Get(id)
	return ok && rec.Status.Resumable()
}

// HandleTaskMessage routes a message in a thread whose task is open
// (TASKS_DESIGN.md §8) and reports whether it did; false means the caller
// continues with the normal flow (intent analysis → possibly a new task).
//
//   - awaiting_input: the message answers the agent's question and resumes
//     the task. A pending plan approval takes yes (proceed), no (ask what to
//     change) or anything else (feedback: revise again).
//   - paused on a budget: "continue" resets the budget and resumes, "stop"
//     cancels, anything else repeats the notice.
//   - paused otherwise: "continue" resumes; anything else is feedback.
//   - done, workspace kept: feedback resumes the task with a plan revision;
//     a new request falls through.
//
// On chat transports (QueueNewRequests) a new request while the task is
// unfinished is queued behind it instead. Small talk ("thanks!") never
// starts a turn: it falls through to the analyzer's normal reply.
func (e *Engine) HandleTaskMessage(ctx context.Context, id string, conv *thread.Thread, text string) bool {
	if _, ok := e.taskStarter(); !ok {
		return false
	}
	rec, ok := e.Tasks.Get(id)
	if !ok {
		return false
	}
	switch rec.Status {
	case task.StatusAwaitingInput:
		if rec.AwaitingApproval {
			switch {
			case IsConfirmation(text) || isContinue(text):
				e.resumeTask(ctx, id, conv, false)
			case IsDenial(text):
				e.say(ctx, id, conv, "OK — what should change in the plan?")
			default:
				e.resumeTask(ctx, id, conv, true)
			}
			return true
		}
		switch e.classify(ctx, rec, text) {
		case thread.TaskMessageChat:
			return false // answered by the analyzer; the question stays open
		case thread.TaskMessageNew:
			if e.QueueNewRequests {
				e.queueRequest(ctx, id, conv, text)
				return true
			}
		}
		e.resumeTask(ctx, id, conv, false)
		return true

	case task.StatusPaused:
		if rec.PauseReason == task.PauseBudget {
			switch {
			case isContinue(text) || IsConfirmation(text):
				_, _ = e.Tasks.Update(id, func(r *task.Record) { r.Budget = task.Budget{} })
				e.resumeTask(ctx, id, conv, false)
			case IsDenial(text):
				e.ResetThread(id)
				e.say(ctx, id, conv, "Stopped the task.")
			default:
				e.say(ctx, id, conv, budgetNotice(rec, e.TaskLimits.Exceeded(rec.Budget)))
			}
			return true
		}
		if isContinue(text) || IsConfirmation(text) {
			e.resumeTask(ctx, id, conv, false)
			return true
		}
		switch e.classify(ctx, rec, text) {
		case thread.TaskMessageChat:
			return false
		case thread.TaskMessageNew:
			if e.QueueNewRequests {
				e.queueRequest(ctx, id, conv, text)
				return true
			}
		}
		e.resumeTask(ctx, id, conv, true)
		return true

	case task.StatusDone:
		if rec.WorkspaceRemoved {
			return false
		}
		if e.classify(ctx, rec, text) != thread.TaskMessageContinue {
			return false // a new request or small talk: the normal flow
		}
		e.resumeTask(ctx, id, conv, true)
		return true
	}
	return false
}

// classify asks the analyzer whether text continues rec's task. Without an
// analyzer: a message in an agent-stream thread continues it (a new request
// would be a new thread); on a chat transport it continues an unfinished
// task but starts a new one after a finished task.
func (e *Engine) classify(ctx context.Context, rec *task.Record, text string) string {
	fallback := thread.TaskMessageContinue
	if e.QueueNewRequests && rec.Status == task.StatusDone {
		fallback = thread.TaskMessageNew
	}
	if e.Analyzer == nil {
		return fallback
	}
	return e.Analyzer.ClassifyTaskMessage(ctx, rec.Goal, rec.OpenQuestion, text, fallback)
}

// isContinue reports a go-ahead to resume a paused task.
func isContinue(text string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!")) {
	case "continue", "go on", "keep going", "resume", "carry on", "go ahead":
		return true
	}
	return false
}

// say replies in the thread and records the reply in its history.
func (e *Engine) say(ctx context.Context, id string, conv *thread.Thread, text string) {
	conv.AddMessage("assistant", text)
	e.Sender.Final(ctx, id, text)
}

// queueRequest queues a new request behind the thread's unfinished task.
func (e *Engine) queueRequest(ctx context.Context, id string, conv *thread.Thread, text string) {
	full := false
	_, _ = e.Tasks.Update(id, func(r *task.Record) {
		if len(r.Queue) >= task.MaxQueue {
			full = true
			return
		}
		r.Queue = append(r.Queue, text)
	})
	if full {
		e.say(ctx, id, conv, fmt.Sprintf("I already have %d requests queued behind the current task — send this again once it finishes, or /cancel the current task.", task.MaxQueue))
		return
	}
	e.say(ctx, id, conv, "Queued — I'll start it when the current task finishes.")
}

// popQueued removes and returns the thread's next queued request, if its
// task is finished.
func (e *Engine) popQueued(id string) string {
	var next string
	_, _ = e.Tasks.Update(id, func(r *task.Record) {
		if r.Status.Finished() && len(r.Queue) > 0 {
			next, r.Queue = r.Queue[0], r.Queue[1:]
		}
	})
	return next
}

// startQueued starts request as a new task in the thread.
func (e *Engine) startQueued(ctx context.Context, id, request string) {
	conv := e.ThreadManager.GetOrCreate(id)
	conv.AddMessage("user", request)
	e.Sender.Reply(ctx, id, "Starting the next queued request: "+request)
	e.HandleConfirmation(ctx, id, conv)
}

// resumeTask starts the task's next turn with the thread's latest message
// as the user's reply or, with feedback, as feedback the planner revises the
// plan for.
func (e *Engine) resumeTask(ctx context.Context, id string, conv *thread.Thread, feedback bool) {
	e.runTurn(ctx, id, conv, feedback)
}

// startTaskTurn starts the thread's next task turn: it continues an open
// task (resumable, or done with feedback) with its context block, or starts
// a new task (releasing a finished predecessor's workspace and inheriting
// its queue). message is the prompt for a new task; request is the user's
// latest message.
func (e *Engine) startTaskTurn(ts TaskStarter, id, message, request string, feedback bool) (string, error) {
	rec, ok := e.Tasks.Get(id)
	continuing := ok && (rec.Status.Resumable() || (feedback && rec.Status == task.StatusDone && !rec.WorkspaceRemoved))
	turn := agent.TaskTurn{Source: e.Source, ConvID: id}
	if continuing {
		turn.Message = task.ContextBlock(rec, request, feedback)
		if feedback {
			turn.Feedback = request
		}
	} else {
		var queue []string
		if ok {
			queue = rec.Queue
			if !rec.WorkspaceRemoved {
				ts.FinishTaskWorkspace(rec.Workspace)
			}
		}
		rec = task.New(id, request, "")
		rec.Workspace = ts.TaskWorkspacePath(rec.ID)
		rec.Queue = queue
		turn.Message = message
	}
	turn.Dir = rec.Workspace

	sessionID, err := ts.StartTaskTurn(turn)
	if err != nil {
		return "", err
	}
	rec.Status = task.StatusWorking
	rec.OpenQuestion = ""
	rec.AwaitingApproval = false
	rec.PauseReason = ""
	rec.Turns = append(rec.Turns, task.Turn{SessionID: sessionID, StartedAt: time.Now(), Request: request})
	if err := e.Tasks.Save(rec); err != nil {
		slog.Warn(e.Label+": could not save task record", "id", id, "task_id", rec.ID, "error", err)
	}
	slog.Info(e.Label+": task turn started", "id", id, "task_id", rec.ID, "turn", len(rec.Turns), "feedback", turn.Feedback != "", "session_id", sessionID)
	return sessionID, nil
}

// finishTaskTurn folds a finished turn into the thread's task record and
// returns the message to post about it: the agent's question, a budget or
// pause notice, or the done checklist ("" for none).
func (e *Engine) finishTaskTurn(id, sessionID string, session *agent.Session, sessionOk bool) (notice string) {
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
				rec.PauseReason = "the run was lost"
			}
			return
		}
		turn.Summary = session.TurnSummary
		turn.CostUSD = session.TotalCostUSD
		rec.Budget.Turns++
		rec.Budget.Seconds += session.ElapsedSeconds
		rec.Budget.CostUSD += session.TotalCostUSD
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
			rec.AwaitingApproval = session.TurnApproval
			turn.StopReason = "needs_input"
			turn.Question = session.TurnQuestion
			if !alreadyAsked(lastOutput(session), session.TurnQuestion) {
				notice = FormatTurnQuestion(session.TurnQuestion)
			}
			return // a question outranks the budget: the user is in the loop
		case completed:
			rec.Status = task.StatusDone
			turn.StopReason = "done"
			notice = doneNotice(rec, e.QueueNewRequests)
			return
		default:
			// A limit, failure or stop: the workspace is kept, so the next
			// message in the thread continues from here.
			rec.Status = task.StatusPaused
			turn.StopReason = orDefault(session.Error, string(session.Status))
			rec.PauseReason = turn.StopReason
		}
		if over := e.TaskLimits.Exceeded(rec.Budget); over != "" {
			rec.PauseReason = task.PauseBudget
			notice = budgetNotice(rec, over)
			return
		}
		notice = pausedNotice(rec)
	})
	if err != nil {
		slog.Warn(e.Label+": could not update task record", "id", id, "error", err)
	}
	if found && release != "" {
		ts.FinishTaskWorkspace(release)
	}
	return notice
}

// withChecklist prefixes text with the task's plan checklist, if any.
func withChecklist(rec *task.Record, text string) string {
	if cl := rec.Checklist(); cl != "" {
		return cl + "\n\n" + text
	}
	return text
}

func doneNotice(rec *task.Record, chat bool) string {
	next := "Reply in this thread with feedback to revise it."
	if chat {
		next = "Reply with feedback to revise it, or send a new request."
	}
	return withChecklist(rec, "✅ Task done. "+next)
}

func pausedNotice(rec *task.Record) string {
	return withChecklist(rec, fmt.Sprintf("⏸ Paused: %s. Reply \"continue\" to pick up where it stopped, or send feedback.", rec.PauseReason))
}

func budgetNotice(rec *task.Record, over string) string {
	return withChecklist(rec, fmt.Sprintf("⏸ Task budget reached (%s). Reply \"continue\" to keep going, or \"stop\" to end the task.", over))
}

// ResetThread cancels the thread's task (if any) and completes the thread —
// the /cancel path. An idle task's workspace is released now; one with a
// turn still running is released when that turn ends. The next queued
// request, if any, then starts.
func (e *Engine) ResetThread(id string) {
	var next string
	if ts, ok := e.taskStarter(); ok {
		var release string
		_, _ = e.Tasks.Update(id, func(rec *task.Record) {
			if rec.Status.Finished() {
				return
			}
			running := rec.Status == task.StatusWorking
			rec.Status = task.StatusCancelled
			rec.OpenQuestion = ""
			rec.AwaitingApproval = false
			if !running && !rec.WorkspaceRemoved {
				release = rec.Workspace
				rec.WorkspaceRemoved = true
			}
		})
		if release != "" {
			ts.FinishTaskWorkspace(release)
		}
		next = e.popQueued(id)
	}
	e.ThreadManager.Complete(id)
	if next != "" {
		e.WG.Go(func() { e.startQueued(context.Background(), id, next) })
	}
}

// lastOutput returns the session's last non-empty iteration output — the
// text the transport already posted as the turn's reply.
func lastOutput(session *agent.Session) string {
	for i := len(session.Iterations) - 1; i >= 0; i-- {
		if out := strings.TrimSpace(session.Iterations[i].Output); out != "" {
			return out
		}
	}
	return ""
}

// alreadyAsked reports whether the agent's posted reply already asks the
// user, so the separate question notice would only repeat it: the reply
// contains the question, or its closing part asks one. The agent often words
// it differently from the question in _progress.json, and may follow it
// with a sentence ("…staging or production? Once you tell me, I'll…").
func alreadyAsked(output, question string) bool {
	if output == "" {
		return false
	}
	norm := func(s string) string {
		return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}), " ")
	}
	if q := norm(question); q != "" && strings.Contains(norm(output), q) {
		return true
	}
	tail := []rune(output)
	if len(tail) > askedTailRunes {
		tail = tail[len(tail)-askedTailRunes:]
	}
	return sentenceQuestion.MatchString(string(tail))
}

// askedTailRunes is how much of the reply's end is checked for a question.
const askedTailRunes = 600

// sentenceQuestion matches a "?" that ends a sentence — not one inside a
// URL's query string.
var sentenceQuestion = regexp.MustCompile(`\?([\s*_)\]"'` + "`" + `]|$)`)

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
