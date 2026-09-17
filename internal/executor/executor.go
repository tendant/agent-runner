package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Executor is the interface for CLI execution backends.
// EnvSetter is implemented by executors that support an environment overlay
// (e.g. an isolation profile's config-dir redirections). Optional so mocks
// and third-party executors stay source-compatible.
type EnvSetter interface {
	SetExtraEnv(env []string)
}

type Executor interface {
	Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error)
	ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error)
	ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error)
	ExecuteWithLogAndSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, string, error)
}

// ClaudeOutput represents the final "result" object from Claude Code CLI.
// Kept for callers that parse --output-format json themselves; the
// executor itself reads stream-json via streamParser.
type ClaudeOutput struct {
	Result       string  `json:"result,omitempty"`
	Error        string  `json:"error,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`       // older CLIs
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"` // current CLIs
	DurationMS   int     `json:"duration_ms,omitempty"`
}

// ExecutionResult contains the result of CLI execution
type ExecutionResult struct {
	Output     string
	RawOutput  string
	Error      error
	CostUSD    float64
	DurationMS int
}

// firstLines returns the first n lines of s, appending "[...truncated]" if more lines follow.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + "\n[...truncated]"
	}
	return s
}

// ClaudeExecutor handles Claude Code CLI execution
type ClaudeExecutor struct {
	// ExtraEnv overlays the inherited environment for spawned processes.
	ExtraEnv []string
	Model    string
	MaxTurns int
}

// NewClaudeExecutor creates a new Claude Code executor
func NewClaudeExecutor(model string, maxTurns int) *ClaudeExecutor {
	return &ClaudeExecutor{Model: model, MaxTurns: maxTurns}
}

// NewExecutor creates an Executor for the given CLI backend.
// Supported values for cli: "claude" (default), "codex", "opencode", "pi".
// For the opencode backend, provider and model are combined as "provider/model"
// when both are non-empty. pi takes provider and model separately; for other
// backends, provider is ignored.
func NewExecutor(cli, provider, model string, maxTurns int) Executor {
	switch cli {
	case "codex":
		return NewCodexExecutor(model)
	case "pi":
		return NewPiExecutor(provider, model)
	case "opencode":
		if provider != "" && model != "" {
			model = provider + "/" + model
		}
		return NewOpencodeExecutor(model, maxTurns)
	default:
		return NewClaudeExecutor(model, maxTurns)
	}
}

// Execute runs Claude Code CLI with the given instruction in the workspace
func (e *ClaudeExecutor) Execute(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, error) {
	return e.ExecuteWithSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithSystemPrompt runs Claude Code CLI with separate system and user prompts.
// If systemPrompt is empty, only the user prompt (instruction) is sent.
func (e *ClaudeExecutor) ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error) {
	return e.ExecuteStreaming(ctx, workspacePath, systemPrompt, instruction, nil)
}

// ExecuteStreaming runs Claude Code with --output-format stream-json and
// reports each tool call and assistant message through onEvent as it
// happens. onEvent may be nil. RawOutput carries a compact transcript of
// those events plus stderr, which is what the audit log wants rather than
// the raw NDJSON.
func (e *ClaudeExecutor) ExecuteStreaming(ctx context.Context, workspacePath, systemPrompt, instruction string, onEvent func(EventKind, string)) (*ExecutionResult, error) {
	// --verbose is required by the CLI for stream-json in --print mode.
	args := []string{"--print", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose"}
	if e.Model != "" {
		args = append(args, "--model", e.Model)
	}
	if e.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(e.MaxTurns))
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	args = append(args, instruction)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workspacePath
	if len(e.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), e.ExtraEnv...)
	}
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

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("CLAUDE_ERROR: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("CLAUDE_ERROR: %w", err)
	}

	parser := newStreamParser(onEvent)
	readErr := parser.consume(stdout)
	err = cmd.Wait()

	result, complete := parser.result()
	if result == nil {
		result = &ExecutionResult{}
	}
	result.RawOutput = parser.transcript.String() + parser.plain.String() + stderr.String()

	if err != nil {
		// Check if it's a context timeout
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}

		// Non-zero exit: prefer the CLI's own result-line error (it names
		// the API status / subtype); otherwise fall back to stderr.
		if result.Error == nil {
			result.Error = fmt.Errorf("CLAUDE_ERROR: %v - %s", err, strings.TrimSpace(stderr.String()))
		}
		return result, result.Error
	}
	if readErr != nil {
		result.Error = fmt.Errorf("CLAUDE_ERROR: reading output: %v", readErr)
		return result, result.Error
	}
	if !complete {
		// Exit 0 with no result line: not stream-json at all (an old CLI,
		// or a wrapper script). Hand back whatever it printed.
		if !parser.sawLine {
			result.Output = strings.TrimSpace(parser.plain.String())
			return result, nil
		}
		result.Error = fmt.Errorf("CLAUDE_ERROR: stream ended without a result line - %s", strings.TrimSpace(stderr.String()))
		return result, result.Error
	}

	return result, nil
}

// ExecuteWithLog runs Claude Code and returns both result and execution log
func (e *ClaudeExecutor) ExecuteWithLog(ctx context.Context, workspacePath, instruction string) (*ExecutionResult, string, error) {
	return e.ExecuteWithLogAndSystemPrompt(ctx, workspacePath, "", instruction)
}

// ExecuteWithLogAndSystemPrompt runs Claude Code with separate system/user prompts
// and returns both result and execution log.
func (e *ClaudeExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, string, error) {
	result, err := e.ExecuteWithSystemPrompt(ctx, workspacePath, systemPrompt, instruction)

	var executionLog string
	if result != nil {
		executionLog = result.RawOutput
	}

	return result, executionLog, err
}

// SetExtraEnv sets an environment overlay applied to spawned processes.
func (e *ClaudeExecutor) SetExtraEnv(env []string) { e.ExtraEnv = env }
