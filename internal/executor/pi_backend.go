package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor/pi"
)

// PiBackend runs the pi coding agent (pi.dev) through its RPC mode as a
// persistent session backend: Start spawns one long-lived
// `pi --mode rpc --no-session` process whose conversation context survives
// across prompts, so later prompts can be incremental.
type PiBackend struct {
	// ExtraEnv overlays the inherited environment for spawned processes.
	ExtraEnv []string
	Provider string
	Model    string
}

// NewPiBackend creates a new pi session backend.
func NewPiBackend(provider, model string) *PiBackend {
	return &PiBackend{Provider: provider, Model: model}
}

// Persistent reports true: one process serves the whole session.
func (b *PiBackend) Persistent() bool { return true }

// SetExtraEnv sets an environment overlay applied to spawned processes.
func (b *PiBackend) SetExtraEnv(env []string) { b.ExtraEnv = env }

// Start spawns the pi process bound to the workspace.
func (b *PiBackend) Start(_ context.Context, workspace string, opts SessionOptions) (Session, error) {
	provider, model := b.Provider, b.Model
	if opts.Provider != "" {
		provider = opts.Provider
	}
	if opts.Model != "" {
		model = opts.Model
	}
	env := append(append([]string{}, b.ExtraEnv...), opts.ExtraEnv...)

	client, err := pi.Start(pi.Options{
		Provider:  provider,
		Model:     model,
		Workspace: workspace,
		Env:       env,
	})
	if err != nil {
		return nil, err
	}
	s := &piSession{client: client, events: make(chan Event, 256)}
	go s.forwardEvents()
	return s, nil
}

// piSession adapts pi.Client to the Session interface.
type piSession struct {
	client *pi.Client
	events chan Event
}

const piCommandTimeout = 10 * time.Second

func (s *piSession) Prompt(ctx context.Context, req PromptRequest) (*ExecutionResult, error) {
	// Pi's RPC prompt has no system-prompt field; prepend it. On persistent
	// use the engine sends SystemPrompt only on the first prompt.
	msg := req.Message
	if req.SystemPrompt != "" {
		msg = req.SystemPrompt + "\n\n" + msg
	}

	res, err := s.client.Prompt(ctx, msg)
	raw := s.client.RawTail() + s.client.StderrTail()
	if err != nil {
		result := &ExecutionResult{RawOutput: raw}
		if errors.Is(err, pi.ErrDead) {
			result.Error = fmt.Errorf("%w: %v", ErrSessionDead, err)
			return result, result.Error
		}
		// Context errors pass through for the caller to classify.
		return result, err
	}
	return &ExecutionResult{Output: res.Text, RawOutput: raw, CostUSD: res.CostUSD}, nil
}

func (s *piSession) Steer(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), piCommandTimeout)
	defer cancel()
	return s.client.Steer(ctx, text)
}

func (s *piSession) FollowUp(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), piCommandTimeout)
	defer cancel()
	return s.client.FollowUp(ctx, text)
}

func (s *piSession) Abort() error {
	ctx, cancel := context.WithTimeout(context.Background(), piCommandTimeout)
	defer cancel()
	return s.client.Abort(ctx)
}

func (s *piSession) Events() <-chan Event { return s.events }

func (s *piSession) Close() error { return s.client.Close() }

// forwardEvents copies pi client events into the session's channel, closing
// it when the client's channel closes (process death or Close).
func (s *piSession) forwardEvents() {
	for ev := range s.client.Events() {
		select {
		case s.events <- Event{Seq: ev.Seq, Kind: EventKind(ev.Kind), Text: ev.Text, At: ev.At}:
		default:
		}
	}
	close(s.events)
}

// PiExecutor adapts the pi backend to the one-shot Executor interface: each
// invocation runs one session with a single prompt. Used by the legacy job
// path, the reviewer, and the fast-LLM fallback.
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
func (e *PiExecutor) ExecuteWithSystemPrompt(ctx context.Context, workspacePath, systemPrompt, instruction string) (*ExecutionResult, error) {
	backend := &PiBackend{Provider: e.Provider, Model: e.Model, ExtraEnv: e.ExtraEnv}
	sess, err := backend.Start(ctx, workspacePath, SessionOptions{})
	if err != nil {
		return nil, fmt.Errorf("PI_ERROR: failed to start pi: %v", err)
	}
	defer sess.Close()

	result, promptErr := sess.Prompt(ctx, PromptRequest{SystemPrompt: systemPrompt, Message: instruction})
	if promptErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("TIMEOUT: execution exceeded timeout")
		}
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("execution was canceled")
		}
		if result == nil {
			result = &ExecutionResult{}
		}
		result.Error = fmt.Errorf("PI_ERROR: %v", promptErr)
		return result, result.Error
	}
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
