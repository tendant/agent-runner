package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CodexExecutor handles OpenAI Codex CLI execution.
type CodexExecutor struct {
	Model string
}

// NewCodexExecutor creates a new Codex CLI executor.
func NewCodexExecutor(model string) *CodexExecutor {
	return &CodexExecutor{Model: model}
}

// Execute runs Codex CLI with the given instruction in the workspace.
func (e *CodexExecutor) Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error) {
	return e.ExecuteWithSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithSystemPrompt runs Codex CLI with separate system and user prompts.
// Codex has no --system-prompt flag, so the system prompt is prepended to the instruction.
func (e *CodexExecutor) ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error) {
	// Create temp file for output
	tmpFile, err := os.CreateTemp("", "codex-output-*.txt")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Build the prompt: prepend system prompt if provided
	prompt := instruction
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n" + instruction
	}

	args := []string{"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"-o", tmpPath,
	}
	if e.Model != "" {
		args = append(args, "-m", e.Model)
	}
	// Pass large prompts over stdin to avoid argv length limits.
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Dir = workspacePath
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// Read output from temp file
	outputData, readErr := os.ReadFile(tmpPath)

	result := &ExecutionResult{
		RawOutput: stdout.String() + stderr.String(),
	}

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}
		result.Error = fmt.Errorf("CODEX_ERROR: %v - %s", runErr, firstLines(stderr.String(), 15))
		return result, result.Error
	}

	if readErr != nil {
		// Output file missing or unreadable — fall back to stdout
		result.Output = stdout.String()
	} else {
		result.Output = strings.TrimSpace(string(outputData))
	}

	return result, nil
}

// ExecuteWithLog runs Codex CLI and returns both result and execution log.
func (e *CodexExecutor) ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error) {
	return e.ExecuteWithLogAndSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithLogAndSystemPrompt runs Codex CLI with separate system/user prompts
// and returns both result and execution log.
func (e *CodexExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, string, error) {
	result, err := e.ExecuteWithSystemPrompt(ctx, workspacePath, systemPrompt, instruction)

	var executionLog string
	if result != nil {
		executionLog = result.RawOutput
	}

	return result, executionLog, err
}
