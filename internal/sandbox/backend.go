package sandbox

import (
	"context"
	"io"
	"os/exec"
	"time"
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
	Tag    string    // opaque label placed in the sandbox launcher's argv for supervisors
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

// Usage is what one prepared sandbox used, as the host measured it.
type Usage struct {
	MemoryPeakBytes int64
	CPUTime         time.Duration
	OOMKills        int64 // processes killed for memory
}

// UsageReporter is implemented by prepared sandboxes the host can measure;
// after Destroy, Usage is what the sandbox used in all (false: unmeasured).
type UsageReporter interface {
	Usage() (Usage, bool)
}

// Commander is implemented by sandboxes that can hand back an unstarted
// *exec.Cmd, for callers needing their own pipes (e.g. persistent stdio agents).
// The returned command is still fully confined; there is no host fallback.
type Commander interface {
	Command(ctx context.Context, p ProcSpec) (*exec.Cmd, error)
}
