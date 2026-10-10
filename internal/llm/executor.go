package llm

import (
	"context"
	"fmt"

	"github.com/agent-runner/agent-runner/internal/executor"
)

// ExecutorClient wraps executor.Executor as a fallback LLM client.
// It invokes the configured CLI (claude/codex) the same way the analyzer
// previously did, preserving existing behavior when no API key is set.
type ExecutorClient struct {
	exec executor.Executor
	// Confine, when set, runs each call that is not already confined (its
	// context carries no executor.Launcher, as inside an agent session) in a
	// sandbox of its own. The fallback runs the agent CLI, with its tools,
	// on a prompt that includes the user's message; with the sandbox on it
	// must not run on the host.
	Confine Confiner
}

// Confiner starts a sandbox run for one call: it returns a context carrying
// the sandbox launcher and a function that ends the run.
type Confiner func(ctx context.Context) (context.Context, func(), error)

func NewExecutorClient(exec executor.Executor) *ExecutorClient {
	return &ExecutorClient{exec: exec}
}

func (c *ExecutorClient) Complete(ctx context.Context, prompt string) (string, error) {
	if c.exec == nil {
		return "", fmt.Errorf("executor: no executor configured")
	}
	if _, confined := executor.LauncherFrom(ctx); c.Confine != nil && !confined {
		cctx, done, err := c.Confine(ctx)
		if err != nil {
			return "", fmt.Errorf("executor: sandbox: %w", err)
		}
		defer done()
		ctx = cctx
	}
	result, err := c.exec.Execute(ctx, "/tmp", prompt)
	if err != nil {
		return "", fmt.Errorf("executor: %w", err)
	}
	output := result.Output
	if output == "" {
		output = result.RawOutput
	}
	return output, nil
}
