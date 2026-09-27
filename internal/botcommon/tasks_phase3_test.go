package botcommon

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/llm"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/task"
	"github.com/agent-runner/agent-runner/internal/thread"
)

func newAnalyzerWith(c llm.Client) *thread.Analyzer { return thread.NewAnalyzer(c) }

// send delivers a user message the way the bots do: add it to the thread,
// then let an open task take it, else run it directly (no analyzer).
func send(f *engineFixture, text string) (handledByTask bool) {
	conv := f.mgr.GetOrCreate("conv-1")
	conv.AddMessage("user", text)
	if f.engine.HandleTaskMessage(context.Background(), "conv-1", conv, text) {
		f.engine.WG.Wait()
		return true
	}
	f.engine.HandleConfirmation(context.Background(), "conv-1", conv)
	f.engine.WG.Wait()
	return false
}

func lastFinal(f *engineFixture) string {
	_, _, finals := f.sender.snapshot()
	if len(finals) == 0 {
		return ""
	}
	return finals[len(finals)-1]
}

func planned(s *agent.Session, done ...bool) *agent.Session {
	plan := &subagent.PlanResult{}
	for i, d := range done {
		id := string(rune('1' + i))
		plan.Steps = append(plan.Steps, subagent.PlanStep{ID: id, Description: "step " + id, Done: d})
	}
	s.PlanJSON = plan
	return s
}

// A large plan revision waits for approval: "no" asks what to change without
// running anything, "yes" continues the task without revising again.
func TestTask_PlanApproval(t *testing.T) {
	f, ts := newTaskFixture(t)
	approval := completedSession()
	approval.TurnStatus = subagent.TurnNeedsInput
	approval.TurnApproval = true
	approval.TurnQuestion = "I've revised the plan … Proceed?"
	ts.setSession(approval)
	send(f, "build it")

	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Status != task.StatusAwaitingInput || !rec.AwaitingApproval {
		t.Fatalf("after the revision: %s approval=%v", rec.Status, rec.AwaitingApproval)
	}
	if n, _ := ts.counts(); n != 1 {
		t.Fatalf("turns = %d", n)
	}

	if !send(f, "no") || !strings.Contains(lastFinal(f), "what should change") {
		t.Fatalf("denial not handled: %q", lastFinal(f))
	}
	if n, _ := ts.counts(); n != 1 {
		t.Fatal("a denial must not start a turn")
	}

	ts.setSession(completedSession("done"))
	if !send(f, "yes") {
		t.Fatal("approval not handled by the task")
	}
	if n, _ := ts.counts(); n != 2 || ts.feedbacks[1] != "" {
		t.Fatalf("turns = %d, feedback %q; want a plain continue", n, ts.feedbacks)
	}
}

// Feedback on a done task (agent-stream: no analyzer, same thread) resumes it
// in its workspace with the feedback for the planner, and the done notice
// carries the checklist.
func TestTask_FeedbackOnDoneTask(t *testing.T) {
	f, ts := newTaskFixture(t)
	ts.setSession(planned(completedSession("built"), true, true))
	send(f, "build it")
	if n := lastFinal(f); !strings.Contains(n, "✅ 1. step 1") || !strings.Contains(n, "Task done") {
		t.Fatalf("done notice = %q", n)
	}

	if !send(f, "also add tests") {
		t.Fatal("feedback on a done task not routed to it")
	}
	_, msgs := ts.counts()
	if len(ts.turnDirs) != 2 || ts.turnDirs[0] != ts.turnDirs[1] || ts.feedbacks[1] != "also add tests" {
		t.Fatalf("dirs %v feedback %q: want the same task continued with feedback", ts.turnDirs, ts.feedbacks)
	}
	if !strings.Contains(msgs[1], "User's feedback on the task (address it): also add tests") {
		t.Fatalf("resume prompt:\n%s", msgs[1])
	}
}

// Reaching a task budget pauses it: other messages repeat the notice,
// "continue" resets the budget and resumes, "stop" cancels.
func TestTask_BudgetPause(t *testing.T) {
	f, ts := newTaskFixture(t)
	f.engine.TaskLimits = task.Limits{MaxTurns: 1}
	failed := completedSession()
	failed.Status = agent.SessionStatusFailed
	failed.Error = "reached max iterations (20)"
	ts.setSession(failed)
	send(f, "big job")

	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Status != task.StatusPaused || rec.PauseReason != task.PauseBudget || !strings.Contains(lastFinal(f), "budget reached (1 turns)") {
		t.Fatalf("after turn 1: %s/%s, notice %q", rec.Status, rec.PauseReason, lastFinal(f))
	}
	send(f, "how is it going?")
	if n, _ := ts.counts(); n != 1 || !strings.Contains(lastFinal(f), "budget reached") {
		t.Fatalf("a non-continue message started a turn or got no notice (%d turns)", n)
	}

	send(f, "continue")
	if n, _ := ts.counts(); n != 2 {
		t.Fatalf("continue did not resume (turns %d)", n)
	}
	if rec, _ = f.engine.Tasks.Get("conv-1"); rec.Budget.Turns != 1 {
		t.Fatalf("budget after continue = %+v, want reset then one turn", rec.Budget)
	}

	send(f, "stop")
	if rec, _ = f.engine.Tasks.Get("conv-1"); rec.Status != task.StatusCancelled {
		t.Fatalf("stop left the task %s", rec.Status)
	}
}

// A limit that isn't the budget pauses with a "continue" notice; any other
// message is feedback.
func TestTask_PausedFeedback(t *testing.T) {
	f, ts := newTaskFixture(t)
	failed := planned(completedSession(), true, false)
	failed.Status = agent.SessionStatusFailed
	failed.Error = "time limit reached"
	ts.setSession(failed)
	send(f, "job")
	if n := lastFinal(f); !strings.Contains(n, "⏸ Paused: time limit reached") || !strings.Contains(n, "⬜ 2. step 2") {
		t.Fatalf("paused notice = %q", n)
	}
	send(f, "skip step 2")
	if ts.feedbacks[1] != "skip step 2" {
		t.Fatalf("feedback = %q", ts.feedbacks)
	}
}

type fixedClassifier struct{ kind string }

func (c fixedClassifier) Complete(context.Context, string) (string, error) {
	return `{"kind": "` + c.kind + `"}`, nil
}

// On a chat transport a new request during an open task is queued, and it
// starts as a new task once the current one finishes.
func TestTask_QueueOnChatTransport(t *testing.T) {
	f, ts := newTaskFixture(t)
	f.engine.QueueNewRequests = true
	ts.setSession(askingSession("Which env?"))
	send(f, "deploy")

	f.engine.Analyzer = newAnalyzerWith(fixedClassifier{"new"})
	if !send(f, "also rename the repo") || !strings.Contains(lastFinal(f), "Queued") {
		t.Fatalf("new request not queued: %q", lastFinal(f))
	}
	if n, _ := ts.counts(); n != 1 {
		t.Fatal("a queued request must not start a turn")
	}

	// The answer finishes the task; the queued request then starts.
	f.engine.Analyzer = newAnalyzerWith(fixedClassifier{"continue"})
	ts.setSession(completedSession("deployed"))
	send(f, "staging")
	f.engine.WG.Wait()

	_, msgs := ts.counts()
	if len(msgs) != 3 {
		t.Fatalf("turns = %d, want answer + queued request", len(msgs))
	}
	if !strings.Contains(msgs[2], "also rename the repo") || ts.turnDirs[2] == ts.turnDirs[1] {
		t.Fatalf("queued request should start a new task:\n%s", msgs[2])
	}
	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Goal != "also rename the repo" || len(rec.Queue) != 0 {
		t.Fatalf("record after dequeue: goal %q queue %v", rec.Goal, rec.Queue)
	}
}

// Small talk after a task never starts a turn or gets queued.
func TestTask_SmallTalkFallsThrough(t *testing.T) {
	f, ts := newTaskFixture(t)
	f.engine.QueueNewRequests = true
	ts.setSession(completedSession("done"))
	send(f, "build it")
	f.engine.Analyzer = newAnalyzerWith(fixedClassifier{"chat"})
	conv := f.mgr.GetOrCreate("conv-1")
	conv.AddMessage("user", "thanks!")
	if f.engine.HandleTaskMessage(context.Background(), "conv-1", conv, "thanks!") {
		t.Fatal("small talk was taken by the task")
	}
	if n, _ := ts.counts(); n != 1 {
		t.Fatalf("turns = %d", n)
	}
	if rec, _ := f.engine.Tasks.Get("conv-1"); len(rec.Queue) != 0 {
		t.Fatalf("small talk queued: %v", rec.Queue)
	}
}

func TestAlreadyAsked(t *testing.T) {
	cases := []struct {
		output, question string
		want             bool
	}{
		{"I've created both files.\n\nWhich of the two is the deploy target — **staging** or **production**?", "Which environment is the deploy target?", true},
		{"Done step 1. Which environment should I use?\n\nI'll wait for your answer.", "Which environment should I use?", true},
		{"Created both.\n\nWhich of the two is the target — **staging** or **production**? Once you tell me, I'll create the file.", "Which environment is the deploy target?", true},
		{"Prepared; need the target environment.", "Which environment should I deploy to?", false},
		{"Opened https://example.com/deploy?env=staging&x=1 for reference.", "Which environment?", false},
		{"", "Which one?", false},
	}
	for _, c := range cases {
		if got := alreadyAsked(c.output, c.question); got != c.want {
			t.Errorf("alreadyAsked(%q) = %v, want %v", c.output, got, c.want)
		}
	}
}

// When the agent's reply already asks, no separate question notice is posted.
func TestTask_NoDuplicateQuestion(t *testing.T) {
	f, ts := newTaskFixture(t)
	s := askingSession("Which environment is the deploy target?")
	s.Iterations[0].Output = "Created both files.\n\nWhich of the two should I deploy — staging or production?"
	ts.setSession(s)
	send(f, "set up the env files, then ask me the target")

	if rec, _ := f.engine.Tasks.Get("conv-1"); rec.Status != task.StatusAwaitingInput {
		t.Fatalf("status %s, want awaiting_input", rec.Status)
	}
	_, _, finals := f.sender.snapshot()
	for _, m := range finals {
		if strings.HasPrefix(m, "❓") {
			t.Fatalf("question posted again as a notice: %q", m)
		}
	}
}
