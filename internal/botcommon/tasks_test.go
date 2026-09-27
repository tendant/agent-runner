package botcommon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/task"
)

// taskStarterFake adds task-turn support to engineStarter and records the
// workspace each turn ran in and which workspaces were released.
type taskStarterFake struct {
	engineStarter
	root       string
	turnDirs   []string
	finishedWS []string
}

func (f *taskStarterFake) StartTaskTurn(message, source, convID, taskDir string) (string, error) {
	f.mu.Lock()
	f.turnDirs = append(f.turnDirs, taskDir)
	f.mu.Unlock()
	return f.StartAgent(message, source, convID)
}

func (f *taskStarterFake) TaskWorkspacePath(key string) string {
	return filepath.Join(f.root, "task-"+key)
}

func (f *taskStarterFake) FinishTaskWorkspace(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishedWS = append(f.finishedWS, dir)
}

func (f *taskStarterFake) setSession(s *agent.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session = s
}

func askingSession(question string) *agent.Session {
	s := completedSession("looked around")
	s.TurnStatus = subagent.TurnNeedsInput
	s.TurnQuestion = question
	s.TurnSummary = "inspected the repo"
	s.TurnDecisions = []string{"use the existing users table"}
	return s
}

func newTaskFixture(t *testing.T) (*engineFixture, *taskStarterFake) {
	t.Helper()
	f := newEngineFixture(t, "")
	ts := &taskStarterFake{root: t.TempDir()}
	ts.session = completedSession("done")
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.engine.Starter = ts
	f.engine.Tasks = store
	return f, ts
}

// A turn that asks a question leaves the task awaiting input and posts the
// question; the reply resumes the same task, in the same workspace, with the
// context block instead of a raw transcript.
func TestTask_NeedsInputThenResume(t *testing.T) {
	f, ts := newTaskFixture(t)
	ts.setSession(askingSession("Which environment?"))

	f.conv.AddMessage("user", "deploy the app")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	rec, ok := f.engine.Tasks.Get("conv-1")
	if !ok {
		t.Fatal("no task record after the first turn")
	}
	if rec.Status != task.StatusAwaitingInput || rec.OpenQuestion != "Which environment?" {
		t.Fatalf("record = %s / %q, want awaiting_input with the question", rec.Status, rec.OpenQuestion)
	}
	if rec.Goal != "deploy the app" || len(rec.Turns) != 1 || rec.Turns[0].Summary != "inspected the repo" {
		t.Fatalf("record goal/turns not recorded: %+v", rec)
	}
	_, _, finals := f.sender.snapshot()
	if len(finals) == 0 || !strings.Contains(finals[len(finals)-1], "Which environment?") {
		t.Fatalf("question not posted, finals = %v", finals)
	}
	if !f.engine.ResumesTask("conv-1") {
		t.Fatal("ResumesTask = false while awaiting input")
	}

	// The user answers.
	ts.setSession(completedSession("deployed"))
	conv := f.mgr.GetOrCreate("conv-1")
	conv.AddMessage("user", "staging")
	f.engine.HandleConfirmation(context.Background(), "conv-1", conv)
	f.engine.WG.Wait()

	_, msgs := ts.counts()
	if len(msgs) != 2 {
		t.Fatalf("started %d turns, want 2", len(msgs))
	}
	resume := msgs[1]
	for _, want := range []string{"## Task", "Goal: deploy the app", "use the existing users table", `asked: "Which environment?"`, "User's reply: staging"} {
		if !strings.Contains(resume, want) {
			t.Errorf("resume prompt missing %q:\n%s", want, resume)
		}
	}
	if strings.Contains(resume, "## Conversation History") {
		t.Error("resume prompt should use the context block, not the transcript")
	}
	if len(ts.turnDirs) != 2 || ts.turnDirs[0] != ts.turnDirs[1] || ts.turnDirs[0] == "" {
		t.Fatalf("turns ran in %v, want the same task workspace twice", ts.turnDirs)
	}

	rec, _ = f.engine.Tasks.Get("conv-1")
	if rec.Status != task.StatusDone || len(rec.Turns) != 2 || rec.OpenQuestion != "" {
		t.Fatalf("after the answer: status %s, %d turns, question %q", rec.Status, len(rec.Turns), rec.OpenQuestion)
	}
	if f.engine.ResumesTask("conv-1") {
		t.Fatal("a done task must not capture the next message")
	}
}

// A new request after a done task starts a new task in a new workspace and
// releases the finished one.
func TestTask_NewTaskAfterDoneReleasesOldWorkspace(t *testing.T) {
	f, ts := newTaskFixture(t)
	for _, text := range []string{"first", "second"} {
		conv := f.mgr.GetOrCreate("conv-1")
		conv.AddMessage("user", text)
		f.engine.HandleConfirmation(context.Background(), "conv-1", conv)
		f.engine.WG.Wait()
	}
	if len(ts.turnDirs) != 2 || ts.turnDirs[0] == ts.turnDirs[1] {
		t.Fatalf("turn dirs = %v, want two different task workspaces", ts.turnDirs)
	}
	if len(ts.finishedWS) != 1 || ts.finishedWS[0] != ts.turnDirs[0] {
		t.Fatalf("released %v, want the first task's workspace", ts.finishedWS)
	}
	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Goal != "second" {
		t.Fatalf("goal = %q, want the new request", rec.Goal)
	}
}

// A failed turn pauses the task so the next message continues it.
func TestTask_FailedTurnPauses(t *testing.T) {
	f, ts := newTaskFixture(t)
	failed := completedSession("partial")
	failed.Status = agent.SessionStatusFailed
	failed.Error = "time limit reached"
	ts.setSession(failed)

	f.conv.AddMessage("user", "big job")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Status != task.StatusPaused || rec.Turns[0].StopReason != "time limit reached" {
		t.Fatalf("status %s / stop %q, want paused on the limit", rec.Status, rec.Turns[0].StopReason)
	}
	if !f.engine.ResumesTask("conv-1") {
		t.Fatal("a paused task should resume on the next message")
	}
}

// /cancel on an idle task cancels it and releases its workspace.
func TestTask_ResetThreadCancels(t *testing.T) {
	f, ts := newTaskFixture(t)
	ts.setSession(askingSession("Which one?"))
	f.conv.AddMessage("user", "pick one")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	f.engine.ResetThread("conv-1")
	rec, _ := f.engine.Tasks.Get("conv-1")
	if rec.Status != task.StatusCancelled || !rec.WorkspaceRemoved {
		t.Fatalf("after reset: %s removed=%v", rec.Status, rec.WorkspaceRemoved)
	}
	if len(ts.finishedWS) != 1 {
		t.Fatalf("released %v, want the task workspace", ts.finishedWS)
	}
	if f.engine.ResumesTask("conv-1") {
		t.Fatal("a cancelled task must not resume")
	}
}

// Without a task store the engine keeps the one-shot path.
func TestTask_DisabledUsesStartAgent(t *testing.T) {
	f := newEngineFixture(t, "")
	ts := &taskStarterFake{root: t.TempDir()}
	ts.session = completedSession("done")
	f.engine.Starter = ts
	f.conv.AddMessage("user", "hi")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()
	if len(ts.turnDirs) != 0 {
		t.Fatalf("task turns started with tasks disabled: %v", ts.turnDirs)
	}
	if n, _ := ts.counts(); n != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", n)
	}
}
