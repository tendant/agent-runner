package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/agent-runner/agent-runner/internal/repos"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
)

type testEnv struct {
	handlers     *Handlers
	repoCacheDir string
	logsDir      string
	tmpDir       string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()

	repoCacheDir := filepath.Join(dir, "repo-cache")

	// Use separate temp dirs for workspaces and logs so background goroutines
	// from executeJob/executeAgent — which keep writing (audit log, memory
	// push retries) after a test returns or calls StopSession — don't race
	// with t.TempDir()'s automatic RemoveAll.
	tmpDir, err := os.MkdirTemp("", "agent-runner-test-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	logsDir, err := os.MkdirTemp("", "agent-runner-test-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(logsDir) })

	os.MkdirAll(repoCacheDir, 0755)

	// Pin DATA_DIR so any config.ReloadFromEnv triggered by /set during tests
	// resolves inside the test sandbox — never the real agent directory.
	t.Setenv("DATA_DIR", dir)

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = repoCacheDir
	cfg.LogsRoot = logsDir
	cfg.TmpRoot = tmpDir
	// Relative so tests that chdir into a temp CWD (withTempCWD) get an
	// isolated memory dir — never the real agent directory.
	cfg.MemoryDir = "memory"
	cfg.OutputsRoot = filepath.Join(dir, "outputs")
	cfg.UploadsRoot = filepath.Join(dir, "uploads")
	cfg.ReposFile = filepath.Join(dir, "repos.json")
	cfg.SecretsFile = filepath.Join(dir, "secrets.json")

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	t.Cleanup(jobManager.Stop)
	agentManager := agent.NewManager(3600, 10, 1)
	t.Cleanup(agentManager.Stop)
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("claude", "", "", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.LogsRoot)

	handlers := NewHandlers(cfg, jobManager, agentManager, gitOps, exec, validator, workspaceManager, runLogger)

	return &testEnv{
		handlers:     handlers,
		repoCacheDir: repoCacheDir,
		logsDir:      logsDir,
		tmpDir:       tmpDir,
	}
}

// createFakeProject registers a repo named name (its URL leads nowhere: the
// tests only look at how a job is accepted).
func (e *testEnv) createFakeProject(t *testing.T, name string) {
	t.Helper()
	repos.AllowLocalPaths = true
	if err := repos.New(e.handlers.config.ReposFile).Put(repos.Repo{Name: name, URL: filepath.Join(e.repoCacheDir, "missing-"+name+".git"), Push: true}); err != nil {
		t.Fatal(err)
	}
}

func postJSON(handler http.HandlerFunc, body interface{}) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func parseJSON(w *httptest.ResponseRecorder) map[string]interface{} {
	var result map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &result)
	return result
}

// --- POST /run validation tests ---

func TestHandleRun_MissingProject(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := parseJSON(w)
	if resp["error"] != "project is required" {
		t.Errorf("unexpected error: %v", resp["error"])
	}
}

func TestHandleRun_MissingInstruction(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project": "test-project",
		"paths":   []string{"src/"},
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := parseJSON(w)
	if resp["error"] != "instruction is required" {
		t.Errorf("unexpected error: %v", resp["error"])
	}
}

func TestHandleRun_MissingPaths(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "fix bug",
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleRun_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/run", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	env := setupTestEnv(t)
	env.handlers.HandleRun(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// A job's project must be a registered repo.
func TestHandleRun_ProjectNotRegistered(t *testing.T) {
	env := setupTestEnv(t)
	os.MkdirAll(filepath.Join(env.repoCacheDir, "nonexistent", ".git"), 0755) // a directory is not enough

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "nonexistent",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if resp := parseJSON(w); !strings.Contains(fmt.Sprint(resp["error"]), "not a registered repo") {
		t.Errorf("unexpected error: %v", resp["error"])
	}
}

func TestHandleRun_MethodNotAllowed(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/run", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleRun(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleRun_Returns202(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", w.Code)
	}

	resp := parseJSON(w)
	if resp["job_id"] == nil || resp["job_id"] == "" {
		t.Error("expected job_id in response")
	}
	if resp["status"] != "queued" {
		t.Errorf("expected status queued, got %v", resp["status"])
	}
}

func TestHandleRun_ProjectLocked(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	// First request succeeds
	w1 := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "task 1",
		"paths":       []string{"src/"},
	})
	if w1.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w1.Code)
	}

	// Second request should get 409
	w2 := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "task 2",
		"paths":       []string{"src/"},
	})
	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w2.Code)
	}
}

func TestHandleRun_AtCapacity(t *testing.T) {
	// Not t.TempDir(): the accepted job keeps running in the background after
	// the test returns and writes its workspace and log, which made
	// t.TempDir()'s cleanup fail ("directory not empty"). This cleanup is
	// best-effort instead.
	dir, err := os.MkdirTemp("", "agent-runner-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	repoCacheDir := filepath.Join(dir, "repo-cache")
	os.MkdirAll(repoCacheDir, 0755)
	tmpDir := filepath.Join(dir, "tmp")

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = repoCacheDir
	cfg.LogsRoot = filepath.Join(dir, "logs")
	cfg.TmpRoot = tmpDir
	cfg.MaxConcurrentJobs = 1
	cfg.ReposFile = filepath.Join(dir, "repos.json")

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	t.Cleanup(jobManager.Stop)
	agentMgr := agent.NewManager(3600, 10, 1)
	t.Cleanup(agentMgr.Stop)
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("claude", "", "", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.LogsRoot)
	handlers := NewHandlers(cfg, jobManager, agentMgr, gitOps, exec, validator, workspaceManager, runLogger)

	// Register two projects
	repos.AllowLocalPaths = true
	for _, n := range []string{"project-a", "project-b"} {
		repos.New(cfg.ReposFile).Put(repos.Repo{Name: n, URL: filepath.Join(dir, n+".git"), Push: true})
	}

	// First request takes the only slot
	w1 := postJSON(handlers.HandleRun, map[string]interface{}{
		"project":     "project-a",
		"instruction": "task 1",
		"paths":       []string{"src/"},
	})
	if w1.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w1.Code)
	}

	// Second request should get 503
	w2 := postJSON(handlers.HandleRun, map[string]interface{}{
		"project":     "project-b",
		"instruction": "task 2",
		"paths":       []string{"src/"},
	})
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w2.Code)
	}
}

// --- GET /job/{id} tests ---

func TestHandleGetJob_Found(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	// Create a job first
	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})
	resp := parseJSON(w)
	jobID := resp["job_id"].(string)

	// Get the job
	req := httptest.NewRequest(http.MethodGet, "/job/"+jobID, nil)
	w2 := httptest.NewRecorder()
	env.handlers.HandleGetJob(w2, req)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w2.Code)
	}

	jobResp := parseJSON(w2)
	if jobResp["job_id"] != jobID {
		t.Errorf("expected job_id %s, got %v", jobID, jobResp["job_id"])
	}
}

func TestHandleGetJob_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/job/nonexistent", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetJob(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetJob_MissingID(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/job/", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetJob(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- GET /status/{project} tests ---

func TestHandleGetStatus_Locked(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	// Lock the project by creating a job
	postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	req := httptest.NewRequest(http.MethodGet, "/status/test-project", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := parseJSON(w)
	if resp["locked"] != true {
		t.Error("expected locked=true")
	}
}

func TestHandleGetStatus_Unlocked(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/status/test-project", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := parseJSON(w)
	if resp["locked"] != false {
		t.Error("expected locked=false")
	}
}

// --- GET /projects tests ---

func TestHandleGetProjects_Empty(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetProjects(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := parseJSON(w)
	projects, ok := resp["projects"].([]interface{})
	if !ok {
		t.Fatal("expected projects array")
	}
	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

func TestHandleGetProjects_WithGitProjects(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "project-a")
	env.createFakeProject(t, "project-b")

	// A directory in the cache is not a project: only registered repos are.
	os.MkdirAll(filepath.Join(env.repoCacheDir, "not-a-repo", ".git"), 0755)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetProjects(w, req)

	resp := parseJSON(w)
	projects := resp["projects"].([]interface{})
	if len(projects) != 2 {
		t.Errorf("expected 2 git projects, got %d", len(projects))
	}
}

func TestHandleGetProjects_ShowsLockStatus(t *testing.T) {
	env := setupTestEnv(t)
	env.createFakeProject(t, "test-project")

	// Lock the project
	postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "test-project",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetProjects(w, req)

	resp := parseJSON(w)
	projects := resp["projects"].([]interface{})
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	proj := projects[0].(map[string]interface{})
	if proj["locked"] != true {
		t.Error("expected project to be locked")
	}
}

// --- Middleware tests ---

func TestApiKeyMiddleware_Valid(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := apiKeyMiddleware("test-key", inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestApiKeyMiddleware_Invalid(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("test-key", inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestApiKeyMiddleware_Missing(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("test-key", inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No X-API-Key header
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// --- Health endpoint tests ---

func TestHandleHealth_ReturnsOK(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	resp := parseJSON(w)
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
}

func TestHandleHealth_MethodNotAllowed(t *testing.T) {
	env := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestApiKeyMiddleware_HealthBypass(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := apiKeyMiddleware("test-key", inner)

	// Request to /health without API key should pass through
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /health without API key, got %d", w.Code)
	}
}
