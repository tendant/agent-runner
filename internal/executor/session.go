package executor

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrUnsupported reports an operation the backend cannot perform (e.g.
	// steering a one-shot CLI).
	ErrUnsupported = errors.New("operation not supported by this backend")
	// ErrSessionDead reports that the backend's process died mid-session.
	ErrSessionDead = errors.New("agent session process died")
)

// SessionOptions configures a run-scoped session. Zero fields fall back to
// the backend's own configuration.
type SessionOptions struct {
	Provider string
	Model    string
	ExtraEnv []string
}

// PromptRequest is one prompt to a session. SystemPrompt is passed to the
// CLI natively where supported and prepended to the message otherwise; on
// persistent backends the engine sends it only on the first prompt.
type PromptRequest struct {
	SystemPrompt string
	Message      string
}

// EventKind classifies session progress events.
type EventKind string

const (
	EventPromptStart EventKind = "prompt_start"
	EventToolStart   EventKind = "tool_start"
	EventToolEnd     EventKind = "tool_end"
	EventRetry       EventKind = "retry"
	EventCompaction  EventKind = "compaction"
	EventSettled     EventKind = "settled"
	EventWarning     EventKind = "warning"
)

// Event is a normalized progress event from a session.
type Event struct {
	Seq  uint64
	Kind EventKind
	Text string
	At   time.Time
}

// Backend creates run-scoped sessions. Implementations also implement
// EnvSetter so the isolation layer can overlay agent-home redirections.
type Backend interface {
	// Start binds a session to a workspace. Persistent backends (pi) spawn
	// one long-lived process here; one-shot adapters only capture options.
	Start(ctx context.Context, workspace string, opts SessionOptions) (Session, error)
	// Persistent reports whether conversation context survives across
	// Prompt calls on one session.
	Persistent() bool
}

// Session is a run-scoped conversation with an agent backend. One prompt
// may be in flight at a time.
type Session interface {
	// Prompt blocks until the backend settles, the context ends, or the
	// session dies (ErrSessionDead).
	Prompt(ctx context.Context, req PromptRequest) (*ExecutionResult, error)
	// Steer injects guidance into the in-flight prompt. ErrUnsupported on
	// one-shot backends.
	Steer(text string) error
	// FollowUp queues a message for delivery after the current prompt
	// settles. ErrUnsupported on one-shot backends.
	FollowUp(text string) error
	// Abort cancels the in-flight prompt. Persistent sessions stay usable.
	Abort() error
	// Events returns the progress event channel, closed when the session
	// ends. Slow consumers lose events rather than blocking the session.
	Events() <-chan Event
	Close() error
}

// NewBackend creates a Backend for the given CLI backend, mirroring
// NewExecutor's resolution: pi is natively session-based; everything else
// wraps the corresponding one-shot Executor.
func NewBackend(cli, provider, model string, maxTurns int) Backend {
	if cli == "pi" {
		return NewPiBackend(provider, model)
	}
	return WrapOneShot(NewExecutor(cli, provider, model, maxTurns))
}

// OneShotBackend adapts a one-shot Executor to the Backend interface: each
// Prompt spawns one CLI process, exactly today's per-iteration behavior.
type OneShotBackend struct {
	exec Executor
}

// WrapOneShot wraps an Executor as a Backend.
func WrapOneShot(exec Executor) *OneShotBackend { return &OneShotBackend{exec: exec} }

// Persistent reports false: every prompt starts a fresh process.
func (b *OneShotBackend) Persistent() bool { return false }

// SetExtraEnv forwards the isolation overlay to the wrapped executor.
func (b *OneShotBackend) SetExtraEnv(env []string) {
	if es, ok := b.exec.(EnvSetter); ok {
		es.SetExtraEnv(env)
	}
}

// Start returns a session bound to the workspace. No process is spawned
// until Prompt.
func (b *OneShotBackend) Start(_ context.Context, workspace string, opts SessionOptions) (Session, error) {
	if len(opts.ExtraEnv) > 0 {
		b.SetExtraEnv(opts.ExtraEnv)
	}
	return &oneShotSession{exec: b.exec, workspace: workspace, events: make(chan Event, 64)}, nil
}

type oneShotSession struct {
	exec      Executor
	workspace string

	events chan Event

	mu     sync.Mutex
	seq    uint64
	closed bool
	cancel context.CancelFunc // cancels the in-flight prompt
}

func (s *oneShotSession) Prompt(ctx context.Context, req PromptRequest) (*ExecutionResult, error) {
	pctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	s.emit(EventPromptStart, "")
	result, _, err := s.exec.ExecuteWithLogAndSystemPrompt(pctx, s.workspace, req.SystemPrompt, req.Message)
	s.emit(EventSettled, "")
	return result, err
}

func (s *oneShotSession) Steer(string) error    { return ErrUnsupported }
func (s *oneShotSession) FollowUp(string) error { return ErrUnsupported }

// Abort cancels the in-flight prompt's context; the CLI process is killed
// through the executor's own Cancel path.
func (s *oneShotSession) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *oneShotSession) Events() <-chan Event { return s.events }

func (s *oneShotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}

func (s *oneShotSession) emit(kind EventKind, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.seq++
	select {
	case s.events <- Event{Seq: s.seq, Kind: kind, Text: text, At: time.Now()}:
	default:
	}
}
