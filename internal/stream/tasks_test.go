package stream

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/task"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// taskStarter asks a question on its first turn and finishes on the second.
type taskStarter struct {
	mu       sync.Mutex
	root     string
	messages []string
	dirs     []string
}

func (s *taskStarter) StartAgent(m, _, _ string) (string, error) {
	return "", errors.New("one-shot path used")
}
func (s *taskStarter) Steer(string, string) error { return errors.New("no steer") }
func (s *taskStarter) StartTaskTurn(turn agent.TaskTurn) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, turn.Message)
	s.dirs = append(s.dirs, turn.Dir)
	if len(s.messages) == 1 {
		return "turn-1", nil
	}
	return "turn-2", nil
}
func (s *taskStarter) TaskWorkspacePath(k string) string { return filepath.Join(s.root, "task-"+k) }
func (s *taskStarter) FinishTaskWorkspace(string)        {}
func (s *taskStarter) GetAgentSession(id string) (*agent.Session, bool) {
	sess := &agent.Session{ID: id, Status: agent.SessionStatusCompleted, MaxTotalSeconds: 1}
	if id == "turn-1" {
		sess.TurnStatus = subagent.TurnNeedsInput
		sess.TurnQuestion = "Which environment?"
	}
	return sess, true
}

type failingAnalyzer struct{}

func (failingAnalyzer) Complete(context.Context, string) (string, error) {
	return "", errors.New("analyzer must not be called for an answer")
}

// In a thread whose task asked a question, the next message is the answer:
// it skips the analyzer and resumes the task in the same workspace.
func TestStreamBot_AnswerResumesTask(t *testing.T) {
	st := &taskStarter{root: t.TempDir()}
	threadMgr := thread.NewManager("")
	t.Cleanup(threadMgr.Stop)
	bot := newTestBot(t, st, nil)
	bot.threadManager = threadMgr
	bot.engine.ThreadManager = threadMgr
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bot.SetTasks(store, task.Limits{})

	// No analyzer on the first message (executes directly), a failing one after.
	bot.handleMessage(context.Background(), "c_1", "m_A", "deploy the app")
	bot.wg.Wait()
	if rec, _ := store.Get("m_A"); rec == nil || rec.Status != task.StatusAwaitingInput {
		t.Fatalf("after turn 1: %+v", rec)
	}

	bot.analyzer = thread.NewAnalyzer(failingAnalyzer{})
	bot.engine.Analyzer = bot.analyzer
	bot.handleMessage(context.Background(), "c_1", "m_A", "staging")
	bot.wg.Wait()

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.messages) != 2 {
		t.Fatalf("started %d turns, want 2", len(st.messages))
	}
	if !strings.Contains(st.messages[1], "User's reply: staging") || st.dirs[0] != st.dirs[1] {
		t.Fatalf("turn 2 did not resume the task: dirs %v\n%s", st.dirs, st.messages[1])
	}
	if rec, _ := store.Get("m_A"); rec.Status != task.StatusDone {
		t.Fatalf("after turn 2: %s", rec.Status)
	}
}
