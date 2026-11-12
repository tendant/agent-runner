package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
)

// The subprocess execution backend end to end (JOBS_DESIGN.md §6, §15.2
// 2e): each session runs in an `agent-runner run-session` child built from
// this tree, against the in-process server under test.
//
// With AGENT_EXECUTION_BACKEND=subprocess the whole e2e package runs that
// way: TestMain builds the runner and baseE2EConfig picks the backend up.

func TestMain(m *testing.M) {
	if os.Getenv("AGENT_EXECUTION_BACKEND") == "subprocess" && os.Getenv("AGENT_JOB_RUNNER_BIN") == "" {
		bin, err := buildRunner()
		if err != nil {
			fmt.Fprintln(os.Stderr, "building the runner:", err)
			os.Exit(1)
		}
		os.Setenv("AGENT_JOB_RUNNER_BIN", bin)
	}
	code := m.Run()
	if runnerDir != "" {
		os.RemoveAll(runnerDir)
	}
	os.Exit(code)
}

var (
	runnerOnce sync.Once
	runnerDir  string
	runnerPath string
	runnerErr  error
)

// buildRunner builds agent-runner once for the package.
func buildRunner() (string, error) {
	runnerOnce.Do(func() {
		runnerDir, runnerErr = os.MkdirTemp("", "e2e-runner-*")
		if runnerErr != nil {
			return
		}
		runnerPath = filepath.Join(runnerDir, "agent-runner")
		cmd := exec.Command("go", "build", "-o", runnerPath, "./cmd/server")
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			runnerErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	return runnerPath, runnerErr
}

func runnerBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("AGENT_JOB_RUNNER_BIN"); bin != "" {
		return bin
	}
	bin, err := buildRunner()
	if err != nil {
		t.Fatalf("building the runner: %v", err)
	}
	return bin
}

// serve serves srv for a test and tells it its URL, which a session runner
// on the subprocess backend reaches it at.
func serve(srv *api.Server) *httptest.Server {
	ts := httptest.NewServer(srv.Handler())
	srv.SetJobDispatcherURL(ts.URL)
	return ts
}

// subprocessEnv is a server on the subprocess backend whose mock claude
// records the runner it ran under (its process group) and, when gated,
// waits for the test to let it finish.
type subprocessEnv struct {
	cfg  *config.Config
	mark string // the mock's marker directory
}

func newSubprocessEnv(t *testing.T, gated bool) *subprocessEnv {
	t.Helper()
	baseDir := t.TempDir()
	repoCacheDir, logsDir, _, mockBinDir := newE2EDirs(t, baseDir)
	tmpDir := newManagedTempDir(t, "e2e-subprocess-tmp-*")
	newGitOrigin(t, baseDir, repoCacheDir, "", "")
	mark := filepath.Join(baseDir, "mark")
	os.MkdirAll(mark, 0o755)

	gate := ""
	if gated {
		gate = fmt.Sprintf(`for i in $(seq 1 600); do [ -f %q/go ] && break; sleep 0.1; done`, mark)
	}
	// Only a session's run (in its workspace) is gated: the server's own
	// model calls fall back to this CLI too.
	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
case "$PWD" in
*/workspace|*/workspace/*)
  touch %[1]q/started
  %[2]s
  ;;
esac
n=$(ls -1 iteration_*.txt 2>/dev/null | wc -l | tr -d ' ')
echo "content" > "iteration_$((n + 1)).txt"
echo '{"result":"Done","cost_usd":0.01,"duration_ms":100}'
`, mark, gate))

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 120
	cfg.StateRoot = filepath.Join(baseDir, "state")
	cfg.Agent.MaxIterations = 2
	cfg.Agent.MaxTotalSeconds = 120
	cfg.Agent.Paths = []string{"*.txt", "*.md"}
	cfg.Agent.ExecutionBackend = "subprocess"
	cfg.Agent.JobRunnerBin = runnerBin(t)
	return &subprocessEnv{cfg: cfg, mark: mark}
}

func (e *subprocessEnv) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(e.mark, "started")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the mock CLI never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runnerPID finds the session's runner process.
func runnerPID(t *testing.T, sessionID string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", "run-session -session "+sessionID+" ").Output()
	fields := strings.Fields(string(out))
	if len(fields) != 1 {
		t.Fatalf("runner processes of %s: %q", sessionID, out)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		t.Fatalf("runner pid %q", out)
	}
	return pid
}

func (e *subprocessEnv) release() { os.WriteFile(filepath.Join(e.mark, "go"), nil, 0o644) }

func startSession(t *testing.T, url string) string {
	t.Helper()
	code, resp := postAgent(t, url, map[string]interface{}{"message": "Create a file"})
	if code != http.StatusAccepted {
		t.Fatalf("POST /agent: %d %v", code, resp)
	}
	return resp["session_id"].(string)
}

// A session runs to completion in a runner process: the CLI ran under
// `run-session`, its commits reached the dispatcher's session, and the
// dispatcher wrote the audit log.
func TestE2E_SubprocessCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	e := newSubprocessEnv(t, true)
	srv := api.NewServer(e.cfg)
	ts := serve(srv)
	defer ts.Close()
	defer srv.StopWork()

	id := startSession(t, ts.URL)
	e.waitStarted(t)
	runnerPID(t, id) // the run is in a runner process
	e.release()
	result := pollAgentUntilDone(t, ts.URL, id, 90*time.Second)
	if st := result["status"]; st != "completed" {
		t.Fatalf("status %v: %v", st, result["error"])
	}
	if its, _ := result["iterations"].([]interface{}); len(its) != 2 {
		t.Errorf("%d iterations, want 2", len(its))
	}
	if logs, _ := filepath.Glob(filepath.Join(e.cfg.LogsRoot, "*"+id+"*")); len(logs) != 1 {
		t.Errorf("%d audit logs for the session", len(logs))
	}
}

// A runner killed mid-run fails its session at once, with why.
func TestE2E_SubprocessKilledRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	e := newSubprocessEnv(t, true)
	srv := api.NewServer(e.cfg)
	ts := serve(srv)
	defer ts.Close()
	defer srv.StopWork()

	id := startSession(t, ts.URL)
	e.waitStarted(t)
	defer e.release() // lets the orphaned mock CLI exit
	if err := syscall.Kill(-runnerPID(t, id), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	result := pollAgentUntilDone(t, ts.URL, id, 30*time.Second)
	errText, _ := result["error"].(string)
	if result["status"] != "failed" || !strings.Contains(errText, "killed") {
		t.Fatalf("status %v error %q", result["status"], errText)
	}
	if logs, _ := filepath.Glob(filepath.Join(e.cfg.LogsRoot, "*"+id+"*")); len(logs) != 1 {
		t.Errorf("%d audit logs for the failed session", len(logs))
	}
}

// The dispatcher goes away mid-run and a new one comes up on the same
// address and state: it re-attaches to the runner, which kept running, and
// finalizes its result (JOBS_DESIGN.md §10).
func TestE2E_SubprocessDispatcherRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}
	e := newSubprocessEnv(t, true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	srv1 := api.NewServer(e.cfg)
	ts1 := httptest.NewUnstartedServer(srv1.Handler())
	ts1.Listener.Close()
	ts1.Listener = ln
	ts1.Start()
	srv1.SetJobDispatcherURL(ts1.URL)

	id := startSession(t, ts1.URL)
	e.waitStarted(t)
	pid := runnerPID(t, id)

	// The first dispatcher goes: its runner's long poll is cut off.
	srv1.StopWork()
	ts1.CloseClientConnections()
	ts1.Close()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the runner died with the dispatcher: %v", err)
	}

	// The second recovers before it serves, as Start does.
	srv2 := api.NewServer(e.cfg)
	srv2.Recover()
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listening again on %s: %v", addr, err)
	}
	ts2 := httptest.NewUnstartedServer(srv2.Handler())
	ts2.Listener.Close()
	ts2.Listener = ln2
	ts2.Start()
	defer ts2.Close()
	defer srv2.StopWork()
	srv2.SetJobDispatcherURL(ts2.URL)

	code, got := getAgent(t, ts2.URL, id)
	if code != http.StatusOK || got["status"] != "running" {
		t.Fatalf("after the restart: %d %v", code, got)
	}
	e.release()
	result := pollAgentUntilDone(t, ts2.URL, id, 90*time.Second)
	if result["status"] != "completed" {
		t.Fatalf("status %v: %v", result["status"], result["error"])
	}
	if its, _ := result["iterations"].([]interface{}); len(its) != 2 {
		t.Errorf("%d iterations, want 2", len(its))
	}
	if ents, _ := os.ReadDir(filepath.Join(e.cfg.StateRoot, "sessions")); len(ents) != 0 {
		t.Errorf("%d journal entries left", len(ents))
	}
}
