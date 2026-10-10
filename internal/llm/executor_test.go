package llm

import (
	"context"
	"testing"

	"github.com/agent-runner/agent-runner/internal/executor"
)

// recordingExecutor reports whether each call's context carried a launcher.
type recordingExecutor struct{ confined []bool }

func (r *recordingExecutor) Execute(ctx context.Context, _, _ string) (*executor.ExecutionResult, error) {
	_, ok := executor.LauncherFrom(ctx)
	r.confined = append(r.confined, ok)
	return &executor.ExecutionResult{Output: "ok"}, nil
}
func (r *recordingExecutor) ExecuteWithSystemPrompt(ctx context.Context, w, _, i string) (*executor.ExecutionResult, error) {
	return r.Execute(ctx, w, i)
}
func (r *recordingExecutor) ExecuteWithLog(ctx context.Context, w, i string) (*executor.ExecutionResult, string, error) {
	res, err := r.Execute(ctx, w, i)
	return res, "", err
}
func (r *recordingExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, w, _, i string) (*executor.ExecutionResult, string, error) {
	return r.ExecuteWithLog(ctx, w, i)
}

func TestExecutorFallbackIsConfinedOutsideASession(t *testing.T) {
	exec := &recordingExecutor{}
	begun, ended := 0, 0
	c := NewExecutorClient(exec)
	c.Confine = func(ctx context.Context) (context.Context, func(), error) {
		begun++
		return executor.WithLauncher(ctx, executor.HostLauncher{}), func() { ended++ }, nil
	}

	// Outside a session (the analyzer): a sandbox run of its own.
	if _, err := c.Complete(context.Background(), "classify this"); err != nil {
		t.Fatal(err)
	}
	// Inside a session (the planner): already confined, run as is.
	inSession := executor.WithLauncher(context.Background(), executor.HostLauncher{})
	if _, err := c.Complete(inSession, "plan this"); err != nil {
		t.Fatal(err)
	}
	if begun != 1 || ended != 1 {
		t.Errorf("sandbox runs begun=%d ended=%d, want 1 and 1", begun, ended)
	}
	if len(exec.confined) != 2 || !exec.confined[0] || !exec.confined[1] {
		t.Errorf("calls confined = %v, want both", exec.confined)
	}
}

func TestNewClientPassesConfineToTheFallback(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"} {
		t.Setenv(k, "")
	}
	confine := Confiner(func(ctx context.Context) (context.Context, func(), error) { return ctx, func() {}, nil })
	c, ok := NewClient(Config{Confine: confine}, &recordingExecutor{}).(*ExecutorClient)
	if !ok || c.Confine == nil {
		t.Fatalf("fallback client = %#v, want an ExecutorClient with Confine", c)
	}
}
