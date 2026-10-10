package isobox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// nprocShim is the argv[1] that turns any binary importing this package into
// a shim: set RLIMIT_NPROC and exec the rest of the argv (see init). gVisor
// runs with a pids cap start the agent CLI through it, because gVisor enforces
// RLIMIT_NPROC inside the sandbox while isobox's --pids caps only the Sentry's
// host threads: a small value stops runsc starting and a fork bomb crashes it
// (docs/sandbox.md "Fork bombs and the memory cap"). A host-side rlimit on
// isobox does not reach the sandbox, so the shim runs inside it.
const nprocShim = "__agent-runner-nproc"

func init() {
	if len(os.Args) > 1 && os.Args[1] == nprocShim {
		err := runNprocShim(os.Args[2:])
		fmt.Fprintln(os.Stderr, "agent-runner nproc shim:", err)
		os.Exit(127)
	}
}

// runNprocShim handles `<self> nprocShim N -- cmd args...`. It returns only on
// error. Soft and hard limits are both N; the sandbox has no capabilities, so
// the command cannot raise them. The limit counts threads as well as processes.
func runNprocShim(args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return fmt.Errorf("usage: %s N -- command [args...]", nprocShim)
	}
	n, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil || n == 0 {
		return fmt.Errorf("bad limit %q", args[0])
	}
	if err := setNproc(n); err != nil {
		return fmt.Errorf("setrlimit RLIMIT_NPROC: %w", err)
	}
	path, err := exec.LookPath(args[2])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args[2:], os.Environ())
}

// nprocPrefix is the argv prefix that runs a command under the shim.
func nprocPrefix(n int64) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("pids cap: locate own binary for the nproc shim: %w", err)
	}
	return []string{self, nprocShim, strconv.FormatInt(n, 10), "--"}, nil
}
