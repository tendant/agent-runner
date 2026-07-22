package botcommon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/conversation"
)

// engineSender records every send, mutex-guarded because the engine's
// post-session goroutine sends concurrently with test assertions.
type engineSender struct {
	mu       sync.Mutex
	statuses []string
	replies  []string
	finals   []string
}

func (s *engineSender) Status(_ context.Context, _, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, text)
}
func (s *engineSender) Reply(_ context.Context, _, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, text)
}
func (s *engineSender) Final(_ context.Context, _, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finals = append(s.finals, text)
}
func (s *engineSender) snapshot() (statuses, replies, finals []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statuses...), append([]string(nil), s.replies...), append([]string(nil), s.finals...)
}

// engineStarter is a controllable AgentStarter: scripted StartAgent results
// and a session that completes immediately (or after gate is closed).
type engineStarter struct {
	mu         sync.Mutex
	startErr   error
	startCalls int
	messages   []string // captured StartAgent messages
	session    *agent.Session
	gate       chan struct{} // when non-nil, GetAgentSession blocks until closed
}

func (f *engineStarter) StartAgent(message, source string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.messages = append(f.messages, message)
	if f.startErr != nil {
		return "", f.startErr
	}
	return fmt.Sprintf("sess-%d", f.startCalls), nil
}

func (f *engineStarter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session, f.session != nil
}

func (f *engineStarter) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls, append([]string(nil), f.messages...)
}

// noopReporter satisfies Reporter for engine tests that don't assert on
// progress reporting (poll_test.go covers PollAndReport itself).
type noopReporter struct{}

func (noopReporter) OnIterationComplete(agent.IterationResult) {}
func (noopReporter) OnIterationStart(int, int)                 {}
func (noopReporter) OnFinal(*agent.Session)                    {}
func (noopReporter) OnNotFound()                               {}
func (noopReporter) OnTimeout()                                {}

// fakeAnalyzerClient returns canned analyzer output.
type fakeAnalyzerClient struct {
	output string
	err    error
	delay  time.Duration
}

func (c *fakeAnalyzerClient) Complete(ctx context.Context, prompt string) (string, error) {
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return c.output, c.err
}

func completedSession(outputs ...string) *agent.Session {
	s := &agent.Session{
		ID:              "sess-1",
		Status:          agent.SessionStatusCompleted,
		MaxTotalSeconds: 1,
	}
	for i, out := range outputs {
		s.Iterations = append(s.Iterations, agent.IterationResult{Iteration: i + 1, Output: out})
	}
	return s
}

type engineFixture struct {
	engine  *Engine
	sender  *engineSender
	starter *engineStarter
	conv    *conversation.Conversation
	mgr     *conversation.Manager
}

func newEngineFixture(t *testing.T, analyzerOutput string) *engineFixture {
	t.Helper()
	withFastPolling(t)

	sender := &engineSender{}
	starter := &engineStarter{session: completedSession("iteration output")}
	mgr := conversation.NewManager(t.TempDir())

	var analyzer *conversation.Analyzer
	if analyzerOutput != "" {
		analyzer = conversation.NewAnalyzer(&fakeAnalyzerClient{output: analyzerOutput})
	}

	e := &Engine{
		Starter:              starter,
		ConvManager:          mgr,
		Analyzer:             analyzer,
		Sender:               sender,
		Source:               "test",
		Label:                "test",
		StartText:            "Starting agent...",
		SessionStartedFormat: "Agent session started: %s",
		AnnounceQueued:       true,
		NewReporter:          func(string) Reporter { return noopReporter{} },
		WG:                   &sync.WaitGroup{},
	}
	return &engineFixture{engine: e, sender: sender, starter: starter, conv: mgr.GetOrCreate("conv-1"), mgr: mgr}
}

// --- HandleConfirmation ---

func TestHandleConfirmation_HappyPath(t *testing.T) {
	f := newEngineFixture(t, "")
	f.conv.AddMessage("user", "do the task")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	statuses, replies, _ := f.sender.snapshot()
	if len(statuses) != 1 || statuses[0] != "Starting agent..." {
		t.Errorf("expected start status, got %v", statuses)
	}
	if len(replies) != 1 || replies[0] != "Agent session started: sess-1" {
		t.Errorf("expected session-started note, got %v", replies)
	}

	// Iteration output folded back into the conversation as assistant turns.
	var sawOutput bool
	for _, m := range f.conv.GetMessages() {
		if m.Role == "assistant" && m.Content == "iteration output" {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Error("expected iteration output added as assistant message")
	}

	// No pending input: conversation completed (state cleared via manager).
	if st := f.conv.GetState(); st == conversation.StateExecuting {
		t.Errorf("state should not remain executing, got %v", st)
	}
}

func TestHandleConfirmation_StartFailure(t *testing.T) {
	f := newEngineFixture(t, "")
	f.starter.startErr = fmt.Errorf("queue is full")
	f.conv.AddMessage("user", "do the task")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)

	_, _, finals := f.sender.snapshot()
	if len(finals) != 1 || !strings.Contains(finals[0], "Failed to start agent: queue is full") {
		t.Errorf("expected failure final, got %v", finals)
	}
	if f.conv.GetState() != conversation.StateGathering {
		t.Errorf("state should reset to gathering, got %v", f.conv.GetState())
	}
}

func TestHandleConfirmation_MessageIncludesHistory(t *testing.T) {
	f := newEngineFixture(t, "")
	f.conv.AddMessage("user", "first request")
	f.conv.AddMessage("assistant", "a reply")
	f.conv.AddMessage("user", "the real task")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	_, messages := f.starter.counts()
	if len(messages) != 1 {
		t.Fatalf("expected 1 StartAgent call, got %d", len(messages))
	}
	if !strings.Contains(messages[0], "## Conversation History") ||
		!strings.Contains(messages[0], "## Current Request\n\nthe real task") {
		t.Errorf("expected history-formatted message, got:\n%s", messages[0])
	}
}

func TestHandleConfirmation_NoStartedNoteWhenFormatEmpty(t *testing.T) {
	f := newEngineFixture(t, "")
	f.engine.SessionStartedFormat = ""
	f.conv.AddMessage("user", "task")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	_, replies, _ := f.sender.snapshot()
	if len(replies) != 0 {
		t.Errorf("expected no session-started note, got %v", replies)
	}
}

func TestHandleConfirmation_OnSessionDoneCalledForOutputFiles(t *testing.T) {
	f := newEngineFixture(t, "")
	f.starter.session.OutputFiles = []agent.OutputFile{{Name: "out.txt"}}
	var mu sync.Mutex
	var uploaded []string
	f.engine.OnSessionDone = func(_ context.Context, id string, session *agent.Session) {
		mu.Lock()
		defer mu.Unlock()
		for _, of := range session.OutputFiles {
			uploaded = append(uploaded, of.Name)
		}
	}
	f.conv.AddMessage("user", "task")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(uploaded) != 1 || uploaded[0] != "out.txt" {
		t.Errorf("expected OnSessionDone with out.txt, got %v", uploaded)
	}
}

func TestHandleConfirmation_PendingInputRedispatches(t *testing.T) {
	f := newEngineFixture(t, "")
	f.starter.gate = make(chan struct{})
	f.conv.AddMessage("user", "task one")

	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)

	// While executing, a new user message arrives → pendingInput set.
	f.conv.AddMessage("user", "task two")
	close(f.starter.gate) // let the session complete

	f.engine.WG.Wait()

	// Analyzer is nil → pending input re-enters HandleConfirmation → a
	// second StartAgent call, preceded by the queued-messages note.
	calls, messages := f.starter.counts()
	if calls != 2 {
		t.Fatalf("expected 2 StartAgent calls (redispatch), got %d", calls)
	}
	if !strings.Contains(messages[1], "task two") {
		t.Errorf("second run should carry the queued message, got:\n%s", messages[1])
	}
	_, replies, _ := f.sender.snapshot()
	var sawQueuedNote bool
	for _, r := range replies {
		if strings.Contains(r, "Processing queued messages") {
			sawQueuedNote = true
		}
	}
	if !sawQueuedNote {
		t.Errorf("expected queued-messages note, got replies %v", replies)
	}
}

// --- HandleAnalysis ---

func TestHandleAnalysis_Ask(t *testing.T) {
	f := newEngineFixture(t, `{"action": "ask", "message": "which repo?"}`)
	f.conv.AddMessage("user", "do something vague")

	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)

	_, _, finals := f.sender.snapshot()
	if len(finals) != 1 || finals[0] != "which repo?" {
		t.Errorf("expected ask message as final, got %v", finals)
	}
	calls, _ := f.starter.counts()
	if calls != 0 {
		t.Error("ask must not start an agent")
	}
}

func TestHandleAnalysis_Plan(t *testing.T) {
	f := newEngineFixture(t, `{"action": "plan", "message": "1. do X\n2. do Y"}`)
	f.conv.AddMessage("user", "complex task")

	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)

	_, _, finals := f.sender.snapshot()
	if len(finals) != 1 || !strings.Contains(finals[0], "Proceed? (yes/no)") {
		t.Errorf("expected plan with proceed prompt, got %v", finals)
	}
	if f.conv.GetPlan() == "" {
		t.Error("expected plan stored on the conversation")
	}
}

func TestHandleAnalysis_ExecuteRunsImmediately(t *testing.T) {
	f := newEngineFixture(t, `{"action": "execute", "message": "On it."}`)
	f.conv.AddMessage("user", "clear task")

	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	_, replies, _ := f.sender.snapshot()
	var sawAck bool
	for _, r := range replies {
		if r == "On it." {
			sawAck = true
		}
	}
	if !sawAck {
		t.Errorf("expected execute ack as reply, got %v", replies)
	}
	calls, _ := f.starter.counts()
	if calls != 1 {
		t.Errorf("execute should start the agent once, got %d calls", calls)
	}
}

func TestHandleAnalysis_UnknownActionTreatedAsAsk(t *testing.T) {
	f := newEngineFixture(t, `{"action": "shrug", "message": "not sure"}`)
	f.conv.AddMessage("user", "task")

	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)

	_, _, finals := f.sender.snapshot()
	if len(finals) != 1 || finals[0] != "not sure" {
		t.Errorf("expected unknown action delivered as final, got %v", finals)
	}
	calls, _ := f.starter.counts()
	if calls != 0 {
		t.Error("unknown action must not start an agent")
	}
}

func TestHandleAnalysis_AnalyzerTimeoutSendsApology(t *testing.T) {
	f := newEngineFixture(t, "unused")
	f.engine.Analyzer = conversation.NewAnalyzer(&fakeAnalyzerClient{delay: time.Second, output: "late"})
	f.engine.Analyzer.SetTimeout(10 * time.Millisecond)
	f.conv.AddMessage("user", "task")

	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)

	_, _, finals := f.sender.snapshot()
	if len(finals) != 1 || !strings.HasPrefix(finals[0], "Sorry,") {
		t.Errorf("expected apology on analyzer timeout, got %v", finals)
	}
	calls, _ := f.starter.counts()
	if calls != 0 {
		t.Error("timeout must not start an agent")
	}
}
