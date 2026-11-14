package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/execution"
)

type startedBackend struct{ started chan execution.Spec }

func (b *startedBackend) Name() string { return "fake" }
func (b *startedBackend) Start(_ context.Context, s execution.Spec) (execution.Handle, error) {
	b.started <- s
	return execution.Handle{Backend: "fake"}, nil
}
func (b *startedBackend) Stop(context.Context, execution.Handle) error { return nil }
func (b *startedBackend) Alive(context.Context, execution.Handle) (bool, string, error) {
	return true, "", nil
}

// The runner's endpoints over HTTP: the token is checked, each op has its
// method, a report and a result go through, and an ended run answers 410.
func TestInternalSessionRoutes(t *testing.T) {
	srv := recoveryServer(t)
	eng := srv.handlers.execEngine
	fb := &startedBackend{started: make(chan execution.Spec, 1)}
	eng.SetBackend(fb)
	s, _ := srv.agentManager.CreateSession("do it", nil, "alice", "", 3, 60)
	go eng.RunQueued(s)
	spec := <-fb.started

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	call := func(method, op, token string, body any) (int, []byte) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, ts.URL+"/internal/sessions/"+s.ID+"/"+op, rd)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	if code, _ := call("GET", "spec", "", nil); code != http.StatusUnauthorized {
		t.Errorf("spec without a token: %d", code)
	}
	if code, _ := call("POST", "spec", spec.Token, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST spec: %d", code)
	}
	if code, _ := call("GET", "nope", spec.Token, nil); code != http.StatusNotFound {
		t.Errorf("unknown op: %d", code)
	}
	code, body := call("GET", "spec", spec.Token, nil)
	var w agent.SessionWire
	if code != http.StatusOK || json.Unmarshal(body, &w) != nil || w.ID != s.ID {
		t.Fatalf("spec %d %s", code, body)
	}

	w.CurrentIteration = 4
	if code, body := call("POST", "report", spec.Token, execution.JobReport{Seq: 1, Session: &w}); code != http.StatusOK {
		t.Fatalf("report %d %s", code, body)
	}
	if got := s.Snapshot().CurrentIteration; got != 4 {
		t.Errorf("report not applied: iteration %d", got)
	}

	if code, body := call("GET", "control?wait=10ms", spec.Token, nil); code != http.StatusOK || string(bytes.TrimSpace(body)) != `{"stop":false}` {
		t.Errorf("control %d %s", code, body)
	}

	runner, _ := agent.NewSessionFromWire(&w, execution.DecodeWire)
	runner.Complete()
	rw, err := (&execution.RunResult{SessionID: s.ID, Source: "api", Message: "do it", StartTime: time.Now(), Snapshot: runner.Snapshot()}).Wire()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := call("POST", "result", spec.Token, rw); code != http.StatusOK {
		t.Fatalf("result %d %s", code, body)
	}
	if st := s.Snapshot().Status; st != agent.SessionStatusCompleted {
		t.Errorf("status %s", st)
	}
	if code, _ := call("GET", "spec", spec.Token, nil); code != http.StatusGone {
		t.Errorf("spec after the result: %d", code)
	}
}

// The runner's routes take its session token, not the API key.
func TestInternalSessionsSkipAPIKey(t *testing.T) {
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc(execution.JobPathPrefix, func(w http.ResponseWriter, r *http.Request) { reached = true })
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := apiKeyMiddleware("k", mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/sessions/agent-1/spec", nil))
	if !reached {
		t.Errorf("internal route refused without the API key: %d", rec.Code)
	}
	reached = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/agent", nil))
	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("/agent passed without the API key: %d", rec.Code)
	}
}

// A runner is built from the dispatcher's configuration, in runner mode,
// recording its sandbox runs apart from the dispatcher's record.
func TestNewRunner(t *testing.T) {
	srv := recoveryServer(t)
	cfg := *srv.handlers.config
	cfg.Agent.SandboxAuditDir = "/var/lib/agent-runner/audit"
	eng := NewRunner(&cfg, "agent-1")
	if !eng.RunnerMode() {
		t.Error("not in runner mode")
	}
	if want := "/var/lib/agent-runner/audit-runners/agent-1"; cfg.Agent.SandboxAuditDir != want {
		t.Errorf("audit dir %q, want %q", cfg.Agent.SandboxAuditDir, want)
	}
}

// A record directory that is a symlink into a shared root puts its runners'
// records beside the record there.
func TestRunnerAuditDirFollowsALink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "shared", "agent-x")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "workdir-audit")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)
	if got, want := RunnerAuditDir(link, "agent-1"), filepath.Join(realResolved+"-runners", "agent-1"); got != want {
		t.Errorf("runner dir %q, want %q", got, want)
	}
}
