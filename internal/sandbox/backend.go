package sandbox

import (
	"context"
	"io"
)

// Backend enforces capabilities; it never owns policy.
type Backend interface {
	// Check reports which required capabilities this backend cannot enforce.
	// It must not start anything.
	Check(ctx context.Context, spec SandboxSpec) (CheckResult, error)
	// Prepare builds a fresh per-run execution boundary.
	Prepare(ctx context.Context, spec SandboxSpec) (PreparedSandbox, error)
}

// ProcSpec is a command to run inside a prepared sandbox. Env is the complete
// environment; nothing is inherited from the host.
type ProcSpec struct {
	Args   []string
	Dir    string // logical path
	Env    []string
	Stdin  io.Reader // must not be a terminal
	Stdout io.Writer
	Stderr io.Writer
}

// PreparedSandbox is one run's boundary. Everything the agent executes goes
// through Run; there is no host fallback.
type PreparedSandbox interface {
	Run(ctx context.Context, p ProcSpec) (exitCode int, err error)
	Destroy(ctx context.Context) error
}

// Gap is a required capability the backend cannot enforce.
type Gap struct {
	Capability CapabilityID `json:"capability"`
	Reason     string       `json:"reason,omitempty"`
}

// CheckResult is the structured outcome of Backend.Check.
type CheckResult struct {
	Backend  string         `json:"backend"`
	Enforced []CapabilityID `json:"enforced"`
	Gaps     []Gap          `json:"gaps,omitempty"`
	Caveats  []string       `json:"caveats,omitempty"`
}
