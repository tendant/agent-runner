package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// OpencodeExecutor handles opencode CLI execution.
// opencode is invoked as: opencode run "<instruction>" --format json
// Output is a newline-delimited JSON (NDJSON) event stream.
type OpencodeExecutor struct {
	Model    string
	MaxTurns int // stored for interface compatibility; opencode has no max-turns flag
}

// NewOpencodeExecutor creates a new opencode CLI executor.
func NewOpencodeExecutor(model string, maxTurns int) *OpencodeExecutor {
	return &OpencodeExecutor{Model: model, MaxTurns: maxTurns}
}

// Execute runs opencode CLI with the given instruction in the workspace.
func (e *OpencodeExecutor) Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error) {
	return e.ExecuteWithSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithSystemPrompt runs opencode CLI with separate system and user prompts.
// opencode has no --system-prompt flag, so the system prompt is prepended to the instruction.
func (e *OpencodeExecutor) ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error) {
	prompt := instruction
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n" + instruction
	}

	args := []string{"run", "--dangerously-skip-permissions", "--format", "json"}
	if e.Model != "" {
		args = append(args, "--model", e.Model)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = workspacePath
	// Put the process in its own group so SIGKILL reaches all children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	// Allow up to 10s for pipes to drain after kill before Wait() forces return.
	cmd.WaitDelay = 10 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecutionResult{
		RawOutput: stdout.String() + stderr.String(),
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}
		result.Error = fmt.Errorf("OPENCODE_ERROR: %v - %s", err, stderr.String())
		return result, result.Error
	}

	output, cost, parseErr := parseOpencodeOutput(stdout.Bytes())
	if parseErr != nil {
		result.Error = parseErr
		return result, parseErr
	}
	result.Output = output
	result.CostUSD = cost
	return result, nil
}

// ExecuteWithLog runs opencode CLI and returns both result and execution log.
func (e *OpencodeExecutor) ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error) {
	return e.ExecuteWithLogAndSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithLogAndSystemPrompt runs opencode CLI with separate system/user prompts
// and returns both result and execution log.
func (e *OpencodeExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, string, error) {
	result, err := e.ExecuteWithSystemPrompt(ctx, workspacePath, systemPrompt, instruction)

	var executionLog string
	if result != nil {
		executionLog = result.RawOutput
	}

	return result, executionLog, err
}

// parseOpencodeOutput extracts the final assistant text and total cost from
// opencode's NDJSON event stream. Text accumulates via message.part.updated
// events; cost is taken from the last session.updated event.
// Returns ("", 0, errMsg) if an error event is found with no text output.
func parseOpencodeOutput(data []byte) (string, float64, error) {
	type errData struct {
		Message string `json:"message"`
	}
	type errInfo struct {
		Name string  `json:"name"`
		Data errData `json:"data"`
	}
	type eventPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type sessionCost struct {
		InputTokens  int     `json:"inputTokens"`
		OutputTokens int     `json:"outputTokens"`
		Cost         float64 `json:"cost"`
	}
	type sessionProps struct {
		Cost sessionCost `json:"cost"`
	}
	type sessionUpdated struct {
		Properties sessionProps `json:"properties"`
	}
	type event struct {
		Type    string         `json:"type"`
		Part    eventPart      `json:"part"`
		Error   errInfo        `json:"error"`
		Session sessionUpdated `json:"session"`
	}

	var lastText string
	var lastErr error
	var totalCost float64
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message.part.updated", "text":
			if ev.Part.Type == "text" && ev.Part.Text != "" {
				lastText = ev.Part.Text
				lastErr = nil
			}
		case "session.updated":
			if c := ev.Session.Properties.Cost.Cost; c > 0 {
				totalCost = c
			}
		case "error":
			msg := ev.Error.Data.Message
			if msg == "" {
				msg = ev.Error.Name
			}
			if msg == "" {
				msg = "unknown opencode error"
			}
			lastErr = fmt.Errorf("OPENCODE_ERROR: %s", msg)
		}
	}

	if lastText != "" {
		return lastText, totalCost, nil
	}
	if lastErr != nil {
		return "", 0, lastErr
	}
	return "", 0, fmt.Errorf("opencode produced no output events (check version and auth)")
}
