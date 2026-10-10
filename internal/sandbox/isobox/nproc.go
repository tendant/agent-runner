package isobox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// nprocShim is the argv[1] that turns any binary importing this package into
// a shim: set RLIMIT_NPROC and exec the rest of the argv (see init). gVisor
// runs with a pids cap start the agent CLI through it, because gVisor enforces
// RLIMIT_NPROC inside the sandbox while isobox's --pids caps only the Sentry's
// host threads: a small value stops runsc starting and a fork bomb crashes it
// (docs/sandbox.md "Fork bombs: memory and pids caps"). A host-side rlimit on
// isobox does not reach the sandbox, so the shim runs inside it, from a copy
// in the run's tmp dir (see nprocPrefix).
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

// shimName is the shim's file name in the run's tmp dir.
const shimName = ".agent-runner-nproc"

// nprocPrefix copies the running binary into tmpDir (the run's tmp root) and
// returns the argv prefix that runs a command under it. The sandbox runs as
// uid 0 without capabilities, so it cannot load the binary where it is
// installed when a parent directory is private (a 0750 home); the run's own
// tmp dir is reachable by design. It is a fresh copy at every launch, never a
// hard link: the sandbox can write its tmp dir, and through a link it could
// rewrite the runner's binary. An agent that replaces the copy during a launch
// only escapes the pids cap of later launches in the same run, not the sandbox.
func nprocPrefix(tmpDir string, n int64) ([]string, error) {
	if tmpDir == "" {
		return nil, errors.New("pids cap: no tmp root for the nproc shim")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("pids cap: locate own binary for the nproc shim: %w", err)
	}
	dst := filepath.Join(tmpDir, shimName)
	if err := copyExecutable(self, dst); err != nil {
		return nil, fmt.Errorf("pids cap: install the nproc shim: %w", err)
	}
	return []string{dst, nprocShim, strconv.FormatInt(n, 10), "--"}, nil
}

func copyExecutable(src, dst string) error {
	// Whatever is there (the agent may have left a symlink or a directory) goes.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
