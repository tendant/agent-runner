package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
)

// SubprocessBackend runs each session as `agent-runner run-session`, a child
// process speaking the job protocol to this server (JOBS_DESIGN.md §6). The
// child gets this server's configuration on its stdin, so it runs with what
// the server runs with (including /config set changes) whatever its
// environment; it shares DATA_DIR. It is in a process group of its own: a
// crash or restart of this server leaves it running, and the next server
// re-attaches to it by its pid (Handle.Ref).
type SubprocessBackend struct {
	h   *Engine
	bin string // the agent-runner binary

	mu    sync.Mutex
	procs map[string]*childProc // by pid, for the children this server started
}

type childProc struct {
	done chan struct{}
	how  string // how it exited, once done
}

// stopKillAfter is how long Stop waits after SIGTERM before SIGKILL.
const stopKillAfter = 10 * time.Second

// NewSubprocessBackend runs sessions with bin (AGENT_JOB_RUNNER_BIN; this
// executable when empty).
func NewSubprocessBackend(h *Engine, bin string) (*SubprocessBackend, error) {
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("finding the agent-runner binary: %w", err)
		}
		bin = exe
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("the runner binary: %w", err)
	}
	return &SubprocessBackend{h: h, bin: bin, procs: map[string]*childProc{}}, nil
}

func (b *SubprocessBackend) Name() string { return "subprocess" }

func (b *SubprocessBackend) Start(_ context.Context, spec Spec) (Handle, error) {
	if spec.ReportURL == "" {
		return Handle{}, errors.New("no dispatcher URL for the runner (AGENT_JOB_DISPATCHER_URL)")
	}
	cfg, err := json.Marshal(b.h.config)
	if err != nil {
		return Handle{}, fmt.Errorf("encoding the configuration: %w", err)
	}
	// Not CommandContext: the run must outlive the queue's context.
	cmd := exec.Command(b.bin, "run-session", "-session", spec.SessionID, "-dispatcher", spec.ReportURL, "-config", "-")
	cmd.Env = append(os.Environ(), "AGENT_JOB_TOKEN="+spec.Token)
	cmd.Stdin = bytes.NewReader(cfg)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Handle{}, err
	}
	pid := cmd.Process.Pid
	ref := strconv.Itoa(pid)
	p := &childProc{done: make(chan struct{})}
	b.mu.Lock()
	b.procs[ref] = p
	b.mu.Unlock()
	slog.Info("session runner started", "session_id", spec.SessionID, "pid", pid)
	go func() {
		err := cmd.Wait()
		how := "exit status 0"
		if err != nil {
			how = err.Error()
		}
		p.how = how
		close(p.done)
		slog.Info("session runner exited", "session_id", spec.SessionID, "pid", pid, "how", how)
		b.h.runnerExited(spec.SessionID, how)
		// The pid may be reused from here on; Alive no longer needs it.
		time.AfterFunc(time.Minute, func() {
			b.mu.Lock()
			delete(b.procs, ref)
			b.mu.Unlock()
		})
	}()
	return Handle{Backend: "subprocess", Ref: ref, Session: spec.SessionID}, nil
}

// Stop ends the runner's process group: SIGTERM, which the runner takes as a
// shutdown (it stops its CLI and still sends the result), then SIGKILL.
func (b *SubprocessBackend) Stop(_ context.Context, h Handle) error {
	pid, err := strconv.Atoi(h.Ref)
	if err != nil || pid <= 1 {
		return fmt.Errorf("bad runner pid %q", h.Ref)
	}
	if alive, _, _ := b.Alive(context.Background(), h); !alive {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	time.AfterFunc(stopKillAfter, func() {
		if alive, _, _ := b.Alive(context.Background(), h); alive {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	return nil
}

// Alive: a child of this server is known exactly. After a restart the pid
// is all there is: it is alive if that process exists and is the runner of
// the session (a reused pid is not).
func (b *SubprocessBackend) Alive(_ context.Context, h Handle) (bool, string, error) {
	b.mu.Lock()
	p, ok := b.procs[h.Ref]
	b.mu.Unlock()
	if ok {
		select {
		case <-p.done:
			return false, p.how, nil
		default:
			return true, "", nil
		}
	}
	pid, err := strconv.Atoi(h.Ref)
	if err != nil || pid <= 1 {
		return false, "", fmt.Errorf("bad runner pid %q", h.Ref)
	}
	return pidIsRunner(pid, h.Session), "", nil
}

// pidIsRunner reports whether pid is a live `run-session` of sessionID.
func pidIsRunner(pid int, sessionID string) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=,command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	if strings.HasPrefix(line, "Z") { // exited, not yet reaped
		return false
	}
	return strings.Contains(line, " run-session ") && strings.Contains(line, "-session "+sessionID+" ")
}

// ReadRunnerConfig reads the configuration a subprocess runner is given on
// its stdin (`run-session -config -`).
func ReadRunnerConfig(r *os.File) (*config.Config, error) {
	var cfg config.Config
	if err := json.NewDecoder(r).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the configuration: %w", err)
	}
	return &cfg, nil
}
