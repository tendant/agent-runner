package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// CodexExecutor handles OpenAI Codex CLI execution.
type CodexExecutor struct {
	// ExtraEnv overlays the inherited environment for spawned processes.
	ExtraEnv []string
	// Launcher starts the CLI; nil runs it directly on the host.
	Launcher Launcher
	Model    string
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
	result, _, err := e.run(ctx, workspacePath, joinPrompt(systemPrompt, instruction), nil, false)
	return result, err
}

// ResumeKind names Codex thread IDs.
func (e *CodexExecutor) ResumeKind() string { return "codex" }

// ExecuteResuming continues the Codex thread req.Ref (`codex exec resume`)
// or, with no ref, starts one and returns its ID (the "thread.started"
// event of --json output). Codex has no system-prompt flag, so the system
// prompt is inlined — except on a continuation, whose thread already has it.
func (e *CodexExecutor) ExecuteResuming(ctx context.Context, workspacePath string, req ResumeRequest) (*ExecutionResult, string, error) {
	prompt := joinPrompt(req.SystemPrompt, req.Instruction)
	if req.Continuation && req.Ref != "" {
		prompt = req.Instruction
	}
	var resume []string
	if req.Ref != "" {
		resume = []string{req.Ref}
	}
	result, threadID, err := e.run(ctx, workspacePath, prompt, resume, true)
	if err != nil && req.Ref != "" && result != nil && strings.Contains(result.RawOutput, "no rollout found") {
		return result, "", fmt.Errorf("%w: %v", ErrResumeFailed, err)
	}
	if threadID == "" {
		threadID = req.Ref
	}
	return result, threadID, err
}

func joinPrompt(systemPrompt, instruction string) string {
	if systemPrompt == "" {
		return instruction
	}
	return systemPrompt + "\n\n" + instruction
}

// run executes one `codex exec` (or, with resume = [thread ID],
// `codex exec resume <id>`). With jsonEvents it asks for --json output and
// returns the thread ID it reports.
func (e *CodexExecutor) run(ctx context.Context, workspacePath, prompt string, resume []string, jsonEvents bool) (*ExecutionResult, string, error) {
	// Create temp file for output
	tmpFile, err := os.CreateTemp("", "codex-output-*.txt")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	args := []string{"exec"}
	if len(resume) > 0 {
		args = append(args, "resume")
	}
	args = append(args,
		"--dangerously-bypass-approvals-and-sandbox",
		"-o", tmpPath,
	)
	if e.Model != "" {
		args = append(args, "-m", e.Model)
	}
	if jsonEvents {
		// --skip-git-repo-check: the task workspace holds repos in
		// subdirectories but is not one itself.
		args = append(args, "--json", "--skip-git-repo-check")
	}
	args = append(args, resume...)
	// Pass large prompts over stdin to avoid argv length limits.
	args = append(args, "-")

	cmd, release, lerr := resolveLauncher(ctx, e.Launcher).Command(ctx, LaunchSpec{Name: "codex", Args: args, Dir: workspacePath, ExtraEnv: e.ExtraEnv})
	if lerr != nil {
		return nil, "", fmt.Errorf("CODEX_ERROR: launch: %w", lerr)
	}
	defer release()
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
	var threadID string
	if jsonEvents {
		threadID = codexThreadID(stdout.String())
	}

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, threadID, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, threadID, fmt.Errorf("execution was canceled")
		}
		result.Error = fmt.Errorf("CODEX_ERROR: %v - %s", runErr, firstLines(stderr.String(), 15))
		return result, threadID, result.Error
	}

	if readErr != nil {
		// Output file missing or unreadable — fall back to stdout
		result.Output = stdout.String()
	} else {
		result.Output = strings.TrimSpace(string(outputData))
	}

	return result, threadID, nil
}

// codexThreadID returns the thread ID from the "thread.started" event of
// `codex exec --json` output, or "".
func codexThreadID(jsonl string) string {
	for _, line := range strings.Split(jsonl, "\n") {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &ev) == nil && ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
	}
	return ""
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

// SetExtraEnv sets an environment overlay applied to spawned processes.
func (e *CodexExecutor) SetExtraEnv(env []string) { e.ExtraEnv = env }

// SetLauncher sets how the CLI is started (host or sandbox).
func (e *CodexExecutor) SetLauncher(l Launcher) { e.Launcher = l }
