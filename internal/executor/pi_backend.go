package executor

import (
	"context"
	"fmt"

	"github.com/agent-runner/agent-runner/internal/executor/pi"
)

// PiExecutor runs the pi coding agent (pi.dev) through its RPC mode: each
// invocation spawns one `pi --mode rpc --no-session` process bound to the
// workspace, sends a single prompt, waits for the agent to settle, and
// collects the final assistant text.
type PiExecutor struct {
	// ExtraEnv overlays the inherited environment for spawned processes.
	ExtraEnv []string
	Provider string
	Model    string
}

// NewPiExecutor creates a new pi executor.
func NewPiExecutor(provider, model string) *PiExecutor {
	return &PiExecutor{Provider: provider, Model: model}
}

// Execute runs pi with the given instruction in the workspace.
func (e *PiExecutor) Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error) {
	return e.ExecuteWithSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithSystemPrompt runs pi with separate system and user prompts.
// Pi's RPC prompt has no system-prompt field, so the system prompt is
// prepended to the instruction.
func (e *PiExecutor) ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error) {
	prompt := instruction
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n" + instruction
	}

	client, err := pi.Start(pi.Options{
		Provider:  e.Provider,
		Model:     e.Model,
		Workspace: workspacePath,
		Env:       e.ExtraEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("PI_ERROR: failed to start pi: %v", err)
	}
	defer client.Close()

	res, promptErr := client.Prompt(ctx, prompt)

	result := &ExecutionResult{RawOutput: client.RawTail() + client.StderrTail()}

	if promptErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}
		result.Error = fmt.Errorf("PI_ERROR: %v - %s", promptErr, firstLines(client.StderrTail(), 15))
		return result, result.Error
	}

	result.Output = res.Text
	result.CostUSD = res.CostUSD
	return result, nil
}

// ExecuteWithLog runs pi and returns both result and execution log.
func (e *PiExecutor) ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error) {
	return e.ExecuteWithLogAndSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithLogAndSystemPrompt runs pi with separate system/user prompts
// and returns both result and execution log.
func (e *PiExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, string, error) {
	result, err := e.ExecuteWithSystemPrompt(ctx, workspacePath, systemPrompt, instruction)

	var executionLog string
	if result != nil {
		executionLog = result.RawOutput
	}

	return result, executionLog, err
}

// SetExtraEnv sets an environment overlay applied to spawned processes.
func (e *PiExecutor) SetExtraEnv(env []string) { e.ExtraEnv = env }
