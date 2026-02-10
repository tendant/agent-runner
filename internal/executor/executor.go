package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// ClaudeOutput represents the JSON output from Claude Code CLI
type ClaudeOutput struct {
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	DurationMS int     `json:"duration_ms,omitempty"`
}

// ExecutionResult contains the result of Claude Code execution
type ExecutionResult struct {
	Output    string
	RawOutput string
	Error     error
}

// Executor handles Claude Code CLI execution
type Executor struct {
	Model    string
	MaxTurns int
}

// NewExecutor creates a new Claude Code executor
func NewExecutor(model string, maxTurns int) *Executor {
	return &Executor{Model: model, MaxTurns: maxTurns}
}

// Execute runs Claude Code CLI with the given instruction in the workspace
func (e *Executor) Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error) {
	args := []string{"--print", "--dangerously-skip-permissions", "--output-format", "json"}
	if e.Model != "" {
		args = append(args, "--model", e.Model)
	}
	if e.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(e.MaxTurns))
	}
	args = append(args, instruction)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workspacePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecutionResult{
		RawOutput: stdout.String() + stderr.String(),
	}

	if err != nil {
		// Check if it's a context timeout
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}

		// Try to parse error from output
		result.Error = fmt.Errorf("CLAUDE_ERROR: %v - %s", err, stderr.String())
		return result, result.Error
	}

	// Try to parse JSON output
	var claudeOut ClaudeOutput
	if err := json.Unmarshal(stdout.Bytes(), &claudeOut); err == nil {
		result.Output = claudeOut.Result
		if claudeOut.Error != "" {
			result.Error = fmt.Errorf("CLAUDE_ERROR: %s", claudeOut.Error)
		}
	} else {
		// Non-JSON output, use raw stdout
		result.Output = stdout.String()
	}

	return result, nil
}

// ExecuteWithLog runs Claude Code and returns both result and execution log
func (e *Executor) ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error) {
	result, err := e.Execute(ctx, workspacePath, instruction)

	var executionLog string
	if result != nil {
		executionLog = result.RawOutput
	}

	return result, executionLog, err
}
