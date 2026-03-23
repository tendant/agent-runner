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
}

func NewExecutorClient(exec executor.Executor) *ExecutorClient {
	return &ExecutorClient{exec: exec}
}

func (c *ExecutorClient) Complete(ctx context.Context, prompt string) (string, error) {
	if c.exec == nil {
		return "", fmt.Errorf("executor: no executor configured")
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
