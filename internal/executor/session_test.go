package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingExecutor captures the arguments of the last execution.
type recordingExecutor struct {
	mu           sync.Mutex
	workspace    string
	systemPrompt string
	instruction  string
	block        bool // block until ctx is done, returning its error
	result       *ExecutionResult
}

func (r *recordingExecutor) get() (ws, sys, instr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workspace, r.systemPrompt, r.instruction
}

func (r *recordingExecutor) Execute(ctx context.Context, ws, instr string) (*ExecutionResult, error) {
	return r.ExecuteWithSystemPrompt(ctx, ws, "", instr)
}

func (r *recordingExecutor) ExecuteWithSystemPrompt(ctx context.Context, ws, sys, instr string) (*ExecutionResult, error) {
	r.mu.Lock()
	r.workspace, r.systemPrompt, r.instruction = ws, sys, instr
	r.mu.Unlock()
	if r.block {
		<-ctx.Done()
		return nil, errors.New("execution was canceled")
	}
	return r.result, nil
}

func (r *recordingExecutor) ExecuteWithLog(ctx context.Context, ws, instr string) (*ExecutionResult, string, error) {
	res, err := r.ExecuteWithSystemPrompt(ctx, ws, "", instr)
	return res, "", err
}

func (r *recordingExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, ws, sys, instr string) (*ExecutionResult, string, error) {
	res, err := r.ExecuteWithSystemPrompt(ctx, ws, sys, instr)
	return res, "", err
}

func TestOneShotBackendPassThrough(t *testing.T) {
	rec := &recordingExecutor{result: &ExecutionResult{Output: "done", CostUSD: 0.5}}
	b := WrapOneShot(rec)
	if b.Persistent() {
		t.Error("one-shot backend must not report persistent")
	}

	sess, err := b.Start(context.Background(), "/ws", SessionOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Close()

	res, err := sess.Prompt(context.Background(), PromptRequest{SystemPrompt: "sys", Message: "do it"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if ws, sys, instr := rec.get(); ws != "/ws" || sys != "sys" || instr != "do it" {
		t.Errorf("pass-through mismatch: ws=%q sys=%q instr=%q", ws, sys, instr)
	}
	if res.Output != "done" || res.CostUSD != 0.5 {
		t.Errorf("result not passed through: %+v", res)
	}

	if err := sess.Steer("x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Steer = %v, want ErrUnsupported", err)
	}
	if err := sess.FollowUp("x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FollowUp = %v, want ErrUnsupported", err)
	}
}

func TestOneShotAbortCancelsPrompt(t *testing.T) {
	rec := &recordingExecutor{block: true}
	b := WrapOneShot(rec)
	sess, err := b.Start(context.Background(), "/ws", SessionOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Close()

	done := make(chan error, 1)
	go func() {
		_, err := sess.Prompt(context.Background(), PromptRequest{Message: "hang"})
		done <- err
	}()

	// Wait for the prompt to be in flight, then abort.
	deadline := time.After(5 * time.Second)
	for {
		if _, _, instr := rec.get(); instr != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("prompt never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := sess.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("aborted prompt should return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return after abort")
	}
}

func TestNewBackendResolution(t *testing.T) {
	if _, ok := NewBackend("pi", "p", "m", 0).(*PiBackend); !ok {
		t.Error("NewBackend(pi) should be *PiBackend")
	}
	for _, cli := range []string{"claude", "codex", "opencode", ""} {
		if _, ok := NewBackend(cli, "", "", 0).(*OneShotBackend); !ok {
			t.Errorf("NewBackend(%q) should be one-shot", cli)
		}
	}
}
