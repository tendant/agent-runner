package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
)

type testEnv struct {
	handlers    *Handlers
	projectsDir string
	runsDir     string
	tmpDir      string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()

	projectsDir := filepath.Join(dir, "projects")
	runsDir := filepath.Join(dir, "runs")
	tmpDir := filepath.Join(dir, "tmp")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)

	cfg := config.DefaultConfig()
	cfg.ProjectsRoot = projectsDir
	cfg.RunsRoot = runsDir
	cfg.TmpRoot = tmpDir

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentManager := agent.NewManager()
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.RunsRoot)

	handlers := NewHandlers(cfg, jobManager, agentManager, gitOps, exec, validator, workspaceManager, runLogger)

	return &testEnv{
		handlers:    handlers,
		projectsDir: projectsDir,
		runsDir:     runsDir,
		tmpDir:      tmpDir,
	}
}

func (e *testEnv) createFakeProject(t *testing.T, name string) {
	t.Helper()
	projectDir := filepath.Join(e.projectsDir, name)
	gitDir := filepath.Join(projectDir, ".git")
	os.MkdirAll(gitDir, 0755)
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

func TestHandleRun_ProjectNotAllowed(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	os.MkdirAll(projectsDir, 0755)

	cfg := config.DefaultConfig()
	cfg.ProjectsRoot = projectsDir
	cfg.RunsRoot = filepath.Join(dir, "runs")
	cfg.TmpRoot = filepath.Join(dir, "tmp")
	cfg.AllowedProjects = []string{"allowed-project"}

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentMgr := agent.NewManager()
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.RunsRoot)
	handlers := NewHandlers(cfg, jobManager, agentMgr, gitOps, exec, validator, workspaceManager, runLogger)

	w := postJSON(handlers.HandleRun, map[string]interface{}{
		"project":     "not-allowed",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := parseJSON(w)
	if resp["error"] != "project not in allowed_projects" {
		t.Errorf("unexpected error: %v", resp["error"])
	}
}

func TestHandleRun_ProjectDirNotFound(t *testing.T) {
	env := setupTestEnv(t)
	// Don't create the project directory

	w := postJSON(env.handlers.HandleRun, map[string]interface{}{
		"project":     "nonexistent",
		"instruction": "fix bug",
		"paths":       []string{"src/"},
	})

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := parseJSON(w)
	if resp["error"] != "project directory not found" {
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
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	os.MkdirAll(projectsDir, 0755)

	cfg := config.DefaultConfig()
	cfg.ProjectsRoot = projectsDir
	cfg.RunsRoot = filepath.Join(dir, "runs")
	cfg.TmpRoot = filepath.Join(dir, "tmp")
	cfg.MaxConcurrentJobs = 1

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentMgr := agent.NewManager()
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.RunsRoot)
	handlers := NewHandlers(cfg, jobManager, agentMgr, gitOps, exec, validator, workspaceManager, runLogger)

	// Create fake projects
	os.MkdirAll(filepath.Join(projectsDir, "project-a", ".git"), 0755)
	os.MkdirAll(filepath.Join(projectsDir, "project-b", ".git"), 0755)

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

	// Also create a non-git directory (should be excluded)
	os.MkdirAll(filepath.Join(env.projectsDir, "not-a-repo"), 0755)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleGetProjects(w, req)

	resp := parseJSON(w)
	projects := resp["projects"].([]interface{})
	if len(projects) != 2 {
		t.Errorf("expected 2 git projects, got %d", len(projects))
	}
}

func TestHandleGetProjects_RespectsAllowlist(t *testing.T) {
	dir := t.TempDir()
	projectsDir := filepath.Join(dir, "projects")
	os.MkdirAll(projectsDir, 0755)

	cfg := config.DefaultConfig()
	cfg.ProjectsRoot = projectsDir
	cfg.RunsRoot = filepath.Join(dir, "runs")
	cfg.TmpRoot = filepath.Join(dir, "tmp")
	cfg.AllowedProjects = []string{"allowed-only"}

	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentMgr := agent.NewManager()
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor("", 0)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.RunsRoot)
	handlers := NewHandlers(cfg, jobManager, agentMgr, gitOps, exec, validator, workspaceManager, runLogger)

	// Create two projects, only one in allowlist
	os.MkdirAll(filepath.Join(projectsDir, "allowed-only", ".git"), 0755)
	os.MkdirAll(filepath.Join(projectsDir, "blocked-project", ".git"), 0755)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	w := httptest.NewRecorder()
	handlers.HandleGetProjects(w, req)

	resp := parseJSON(w)
	projects := resp["projects"].([]interface{})
	if len(projects) != 1 {
		t.Errorf("expected 1 allowed project, got %d", len(projects))
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
