package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
)

func setupAgentE2E(t *testing.T) *e2eEnv {
	t.Helper()
	baseDir := t.TempDir()

	bareRepo := filepath.Join(baseDir, "origin.git")
	projectsDir := filepath.Join(baseDir, "projects")
	runsDir := filepath.Join(baseDir, "runs")
	tmpDir := filepath.Join(baseDir, "tmp")
	mockBinDir := filepath.Join(baseDir, "mock-bin")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)
	os.MkdirAll(mockBinDir, 0755)

	// 1. Create bare repo
	runCmd(t, "", "git", "init", "--bare", bareRepo)

	// 2. Clone, initial commit, push
	projectPath := filepath.Join(projectsDir, "test-project")
	runCmd(t, "", "git", "clone", bareRepo, projectPath)
	runCmd(t, projectPath, "git", "config", "user.email", "test@test.com")
	runCmd(t, projectPath, "git", "config", "user.name", "Test")

	os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("# Test Project\n"), 0644)
	runCmd(t, projectPath, "git", "add", "-A")
	runCmd(t, projectPath, "git", "commit", "-m", "Initial commit")
	runCmd(t, projectPath, "git", "push", "origin", "HEAD")

	// 3. Create mock claude that tracks invocation count via a counter file.
	// Odd invocations (1, 3, 5) write a file; even invocations (2, 4) produce no changes.
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	mockClaude := filepath.Join(mockBinDir, "claude")
	mockScript := fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"

# Odd iterations produce files; even iterations are no-ops
if [ $((next %% 2)) -eq 1 ]; then
    echo "content for iteration $next" > "iteration_${next}.txt"
fi

echo '{"result":"Done","cost_usd":0.01,"duration_ms":500}'
`, counterFile)

	os.WriteFile(mockClaude, []byte(mockScript), 0755)

	// 4. Prepend mock dir to PATH
	os.Setenv("PATH", mockBinDir+":"+os.Getenv("PATH"))

	// 5. Config with agent settings
	cfg := &config.Config{
		ProjectsRoot:             projectsDir,
		RunsRoot:                 runsDir,
		TmpRoot:                  tmpDir,
		AllowedProjects:          []string{},
		MaxRuntimeSeconds:        60,
		MaxConcurrentJobs:        5,
		GitPushRetries:           1,
		GitPushRetryDelaySeconds: 0,
		Validation: config.ValidationConfig{
			BlockBinaryFiles: false,
			BlockedPaths:     []string{},
		},
		API: config.APIConfig{
			Bind:   "127.0.0.1:0",
			APIKey: "",
		},
		Agent: config.AgentConfig{
			MaxIterations:       5,
			MaxTotalSeconds:     60,
			MaxIterationSeconds: 30,
			Paths:               []string{"*.txt", "*.md"},
			Author:              "test-agent",
			CommitPrefix:        "[agent]",
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())

	return &e2eEnv{
		server:      ts,
		bareRepo:    bareRepo,
		projectsDir: projectsDir,
		runsDir:     runsDir,
		tmpDir:      tmpDir,
		mockBinDir:  mockBinDir,
	}
}

func postAgent(t *testing.T, serverURL string, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(serverURL+"/agent", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /agent failed: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result
}

func getAgent(t *testing.T, serverURL, sessionID string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(serverURL + "/agent/" + sessionID)
	if err != nil {
		t.Fatalf("GET /agent failed: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result
}

func stopAgent(t *testing.T, serverURL, sessionID string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Post(serverURL+"/agent/"+sessionID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /agent/stop failed: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result
}

func pollAgentUntilDone(t *testing.T, serverURL, sessionID string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, result := getAgent(t, serverURL, sessionID)
		status, _ := result["status"].(string)
		if status == "completed" || status == "failed" {
			return result
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("agent %s did not complete within %v", sessionID, timeout)
	return nil
}

func TestE2E_AgentMultiIteration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	env := setupAgentE2E(t)
	defer env.server.Close()

	// Start agent with simplified message-only request
	code, resp := postAgent(t, env.server.URL, map[string]interface{}{
		"message": "Write a new file with some content.",
	})

	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID, ok := resp["session_id"].(string)
	if !ok || sessionID == "" {
		t.Fatal("expected session_id in response")
	}

	// Poll until done
	result := pollAgentUntilDone(t, env.server.URL, sessionID, 60*time.Second)

	status, _ := result["status"].(string)
	if status != "completed" {
		t.Fatalf("expected completed, got %s: error=%v", status, result["error"])
	}

	// Verify iterations — simplified loop returns success for every iteration
	iterations, ok := result["iterations"].([]interface{})
	if !ok {
		t.Fatal("expected iterations in response")
	}
	if len(iterations) != 5 {
		t.Fatalf("expected 5 iterations, got %d", len(iterations))
	}

	for i, iter := range iterations {
		iterMap := iter.(map[string]interface{})
		iterStatus := iterMap["status"].(string)
		if iterStatus != "success" {
			t.Errorf("iteration %d: expected status success, got %s", i+1, iterStatus)
		}
	}

	// Verify audit log was written
	entries, err := os.ReadDir(env.runsDir)
	if err != nil {
		t.Fatalf("failed to read runs dir: %v", err)
	}
	foundAgentLog := false
	for _, entry := range entries {
		if !entry.IsDir() && len(entry.Name()) > 0 {
			foundAgentLog = true
		}
	}
	if !foundAgentLog {
		t.Error("expected agent audit log file")
	}
}

func TestE2E_AgentGracefulStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()

	bareRepo := filepath.Join(baseDir, "origin.git")
	projectsDir := filepath.Join(baseDir, "projects")
	runsDir := filepath.Join(baseDir, "runs")
	tmpDir := filepath.Join(baseDir, "tmp")
	mockBinDir := filepath.Join(baseDir, "mock-bin")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)
	os.MkdirAll(mockBinDir, 0755)

	runCmd(t, "", "git", "init", "--bare", bareRepo)

	projectPath := filepath.Join(projectsDir, "test-project")
	runCmd(t, "", "git", "clone", bareRepo, projectPath)
	runCmd(t, projectPath, "git", "config", "user.email", "test@test.com")
	runCmd(t, projectPath, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("# Test\n"), 0644)
	runCmd(t, projectPath, "git", "add", "-A")
	runCmd(t, projectPath, "git", "commit", "-m", "Initial")
	runCmd(t, projectPath, "git", "push", "origin", "HEAD")

	// Slow mock claude
	mockClaude := filepath.Join(mockBinDir, "claude")
	mockScript := `#!/bin/bash
sleep 1
count=$(ls -1 iteration_*.txt 2>/dev/null | wc -l | tr -d ' ')
next=$((count + 1))
echo "content" > "iteration_${next}.txt"
echo '{"result":"Done"}'
`
	os.WriteFile(mockClaude, []byte(mockScript), 0755)
	os.Setenv("PATH", mockBinDir+":"+os.Getenv("PATH"))

	cfg := &config.Config{
		ProjectsRoot:    projectsDir,
		RunsRoot:        runsDir,
		TmpRoot:         tmpDir,
		AllowedProjects: []string{},
		MaxRuntimeSeconds: 60,
		MaxConcurrentJobs: 5,
		GitPushRetries:    1,
		GitPushRetryDelaySeconds: 0,
		Validation: config.ValidationConfig{
			BlockBinaryFiles: false,
			BlockedPaths:     []string{},
		},
		API: config.APIConfig{
			Bind:   "127.0.0.1:0",
			APIKey: "",
		},
		Agent: config.AgentConfig{
			MaxIterations:       50,
			MaxTotalSeconds:     60,
			MaxIterationSeconds: 30,
			Paths:               []string{"*.txt", "*.md"},
			Author:              "claude-agent",
			CommitPrefix:        "[agent]",
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Start agent with simplified message-only request
	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Create a file",
	})

	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID := resp["session_id"].(string)

	// Wait for at least one iteration to start
	time.Sleep(2 * time.Second)

	// Send stop
	stopCode, stopResp := stopAgent(t, ts.URL, sessionID)
	if stopCode != http.StatusOK {
		t.Fatalf("expected 200 for stop, got %d: %v", stopCode, stopResp)
	}

	// Poll until done
	result := pollAgentUntilDone(t, ts.URL, sessionID, 30*time.Second)

	status, _ := result["status"].(string)
	if status != "completed" {
		t.Fatalf("expected completed after stop, got %s", status)
	}

	// Should have fewer than 50 iterations (stopped early)
	iterations, _ := result["iterations"].([]interface{})
	if len(iterations) >= 50 {
		t.Error("expected agent to stop before reaching max iterations")
	}
	if len(iterations) == 0 {
		t.Error("expected at least one iteration before stop")
	}
}

func TestE2E_AgentWithPromptTemplate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()

	bareRepo := filepath.Join(baseDir, "origin.git")
	projectsDir := filepath.Join(baseDir, "projects")
	runsDir := filepath.Join(baseDir, "runs")
	tmpDir := filepath.Join(baseDir, "tmp")
	mockBinDir := filepath.Join(baseDir, "mock-bin")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)
	os.MkdirAll(mockBinDir, 0755)

	runCmd(t, "", "git", "init", "--bare", bareRepo)

	projectPath := filepath.Join(projectsDir, "test-project")
	runCmd(t, "", "git", "clone", bareRepo, projectPath)
	runCmd(t, projectPath, "git", "config", "user.email", "test@test.com")
	runCmd(t, projectPath, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("# Test\n"), 0644)
	runCmd(t, projectPath, "git", "add", "-A")
	runCmd(t, projectPath, "git", "commit", "-m", "Initial")
	runCmd(t, projectPath, "git", "push", "origin", "HEAD")

	// Mock claude that creates a file
	mockClaude := filepath.Join(mockBinDir, "claude")
	mockScript := `#!/bin/bash
echo "content" > "output.txt"
echo '{"result":"Done"}'
`
	os.WriteFile(mockClaude, []byte(mockScript), 0755)
	os.Setenv("PATH", mockBinDir+":"+os.Getenv("PATH"))

	// Create prompt template file
	promptFile := filepath.Join(baseDir, "prompt.md")
	os.WriteFile(promptFile, []byte("You are a helpful assistant.\n\nTask: {{MESSAGE}}"), 0644)

	cfg := &config.Config{
		ProjectsRoot:             projectsDir,
		RunsRoot:                 runsDir,
		TmpRoot:                  tmpDir,
		AllowedProjects:          []string{},
		MaxRuntimeSeconds:        60,
		MaxConcurrentJobs:        5,
		GitPushRetries:           1,
		GitPushRetryDelaySeconds: 0,
		Validation: config.ValidationConfig{
			BlockBinaryFiles: false,
			BlockedPaths:     []string{},
		},
		API: config.APIConfig{
			Bind:   "127.0.0.1:0",
			APIKey: "",
		},
		Agent: config.AgentConfig{
			MaxIterations:       1,
			MaxTotalSeconds:     60,
			MaxIterationSeconds: 30,
			Paths:               []string{"*.txt", "*.md"},
			Author:              "claude-agent",
			CommitPrefix:        "[agent]",
			PromptFile:          promptFile,
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Fix the login bug",
	})
	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID := resp["session_id"].(string)
	result := pollAgentUntilDone(t, ts.URL, sessionID, 30*time.Second)

	status, _ := result["status"].(string)
	if status != "completed" {
		t.Fatalf("expected completed, got %s: error=%v", status, result["error"])
	}
}

func TestE2E_AgentNoProjectNoDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()
	projectsDir := filepath.Join(baseDir, "projects")
	runsDir := filepath.Join(baseDir, "runs")
	tmpDir := filepath.Join(baseDir, "tmp")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)

	cfg := &config.Config{
		ProjectsRoot:    projectsDir,
		RunsRoot:        runsDir,
		TmpRoot:         tmpDir,
		AllowedProjects: []string{},
		MaxRuntimeSeconds: 60,
		MaxConcurrentJobs: 5,
		Agent: config.AgentConfig{
			MaxIterations:   1,
			MaxTotalSeconds: 60,
			Paths:           []string{"*.txt"},
		},
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// No project concept — should work with just a message
	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "do stuff",
	})

	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID, _ := resp["session_id"].(string)
	if sessionID == "" {
		t.Error("expected session_id in response")
	}
}

func TestE2E_AgentWithPlanner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()

	bareRepo := filepath.Join(baseDir, "origin.git")
	projectsDir := filepath.Join(baseDir, "projects")
	runsDir := filepath.Join(baseDir, "runs")
	tmpDir := filepath.Join(baseDir, "tmp")
	mockBinDir := filepath.Join(baseDir, "mock-bin")

	os.MkdirAll(projectsDir, 0755)
	os.MkdirAll(runsDir, 0755)
	os.MkdirAll(tmpDir, 0755)
	os.MkdirAll(mockBinDir, 0755)

	runCmd(t, "", "git", "init", "--bare", bareRepo)

	projectPath := filepath.Join(projectsDir, "test-project")
	runCmd(t, "", "git", "clone", bareRepo, projectPath)
	runCmd(t, projectPath, "git", "config", "user.email", "test@test.com")
	runCmd(t, projectPath, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("# Test\n"), 0644)
	runCmd(t, projectPath, "git", "add", "-A")
	runCmd(t, projectPath, "git", "commit", "-m", "Initial")
	runCmd(t, projectPath, "git", "push", "origin", "HEAD")

	// Mock claude that returns plan JSON on invocation 1, normal execution afterwards
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	mockClaude := filepath.Join(mockBinDir, "claude")
	mockScript := fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"

if [ "$next" -eq 1 ]; then
    # First invocation is the planner — return plan JSON
    echo '{"result":"{\"summary\":\"Test plan\",\"steps\":[{\"id\":\"1\",\"description\":\"Create file\",\"done\":false},{\"id\":\"2\",\"description\":\"Add content\",\"done\":false}],\"approach\":\"Direct implementation\"}"}'
else
    # Subsequent invocations are iteration work
    echo "content for iteration $next" > "work_${next}.txt"
    echo '{"result":"Done","cost_usd":0.01,"duration_ms":500}'
fi
`, counterFile)

	os.WriteFile(mockClaude, []byte(mockScript), 0755)
	os.Setenv("PATH", mockBinDir+":"+os.Getenv("PATH"))

	cfg := &config.Config{
		ProjectsRoot:             projectsDir,
		RunsRoot:                 runsDir,
		TmpRoot:                  tmpDir,
		AllowedProjects:          []string{},
		MaxRuntimeSeconds:        60,
		MaxConcurrentJobs:        5,
		GitPushRetries:           1,
		GitPushRetryDelaySeconds: 0,
		Validation: config.ValidationConfig{
			BlockBinaryFiles: false,
			BlockedPaths:     []string{},
		},
		API: config.APIConfig{
			Bind:   "127.0.0.1:0",
			APIKey: "",
		},
		Agent: config.AgentConfig{
			MaxIterations:       3,
			MaxTotalSeconds:     60,
			MaxIterationSeconds: 30,
			Paths:               []string{"*.txt", "*.md"},
			Author:              "test-agent",
			CommitPrefix:        "[agent]",
			PlannerEnabled:      true,
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Create some test files",
	})

	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID := resp["session_id"].(string)
	result := pollAgentUntilDone(t, ts.URL, sessionID, 60*time.Second)

	status, _ := result["status"].(string)
	if status != "completed" {
		t.Fatalf("expected completed, got %s: error=%v", status, result["error"])
	}

	// Verify plan is included in the response
	planData, hasPlan := result["plan"]
	if !hasPlan {
		t.Fatal("expected plan in response")
	}

	planMap, ok := planData.(map[string]interface{})
	if !ok {
		t.Fatalf("expected plan to be a map, got %T", planData)
	}

	if summary, _ := planMap["summary"].(string); summary != "Test plan" {
		t.Errorf("expected plan summary 'Test plan', got '%s'", summary)
	}

	steps, ok := planMap["steps"].([]interface{})
	if !ok || len(steps) != 2 {
		t.Errorf("expected 2 plan steps, got %v", planMap["steps"])
	}

	// Verify iterations ran (should be 3 since planner is separate)
	iterations, ok := result["iterations"].([]interface{})
	if !ok {
		t.Fatal("expected iterations in response")
	}
	if len(iterations) != 3 {
		t.Fatalf("expected 3 iterations, got %d", len(iterations))
	}

	// Verify audit log was written and contains plan.
	// The log is written in a defer after CompleteSession, so retry briefly.
	var logContent string
	for retry := 0; retry < 10; retry++ {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			t.Fatalf("failed to read runs dir: %v", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				data, _ := os.ReadFile(filepath.Join(runsDir, entry.Name()))
				logContent = string(data)
				break
			}
		}
		if logContent != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if logContent == "" {
		t.Fatal("expected agent audit log file")
	}
	if !strings.Contains(logContent, "## Plan") {
		t.Error("expected Plan section in audit log")
	}
}
