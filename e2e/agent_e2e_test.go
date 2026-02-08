package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// Create AGENT_PROMPT.md in the project
	os.WriteFile(filepath.Join(projectPath, "AGENT_PROMPT.md"), []byte("Write a new file with some content."), 0644)

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
			MaxIterations:       10,
			MaxTotalSeconds:     60,
			MaxIterationSeconds: 30,
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

	// Start agent with max 5 iterations
	code, resp := postAgent(t, env.server.URL, map[string]interface{}{
		"project":       "test-project",
		"prompt_file":   "AGENT_PROMPT.md",
		"paths":         []string{"*.txt", "*.md"},
		"max_iterations": 5,
		"author":        "test-agent",
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

	// Verify iterations
	iterations, ok := result["iterations"].([]interface{})
	if !ok {
		t.Fatal("expected iterations in response")
	}
	if len(iterations) != 5 {
		t.Fatalf("expected 5 iterations, got %d", len(iterations))
	}

	// Verify iteration statuses: 1=success, 2=no_changes, 3=success, 4=no_changes, 5=success
	expectedStatuses := []string{"success", "no_changes", "success", "no_changes", "success"}
	for i, iter := range iterations {
		iterMap := iter.(map[string]interface{})
		iterStatus := iterMap["status"].(string)
		if iterStatus != expectedStatuses[i] {
			t.Errorf("iteration %d: expected status %s, got %s", i+1, expectedStatuses[i], iterStatus)
		}
	}

	// Verify total commits (iterations 1, 3, 5 produced commits)
	totalCommits, _ := result["total_commits"].(float64)
	if int(totalCommits) != 3 {
		t.Errorf("expected 3 total commits, got %d", int(totalCommits))
	}

	// Verify files landed in bare repo
	cloneDir := filepath.Join(t.TempDir(), "verify")
	runCmd(t, "", "git", "clone", env.bareRepo, cloneDir)

	for _, n := range []int{1, 3, 5} {
		fname := fmt.Sprintf("iteration_%d.txt", n)
		if _, err := os.Stat(filepath.Join(cloneDir, fname)); os.IsNotExist(err) {
			t.Errorf("expected %s to be pushed to origin", fname)
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
	os.WriteFile(filepath.Join(projectPath, "AGENT_PROMPT.md"), []byte("Create a file"), 0644)
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
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Start agent with many iterations
	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"project":        "test-project",
		"prompt_file":    "AGENT_PROMPT.md",
		"paths":          []string{"*.txt", "*.md"},
		"max_iterations": 50,
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

func TestE2E_AgentProjectLocking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	env := setupAgentE2E(t)
	defer env.server.Close()

	// Start first agent
	code, resp := postAgent(t, env.server.URL, map[string]interface{}{
		"project":        "test-project",
		"prompt_file":    "AGENT_PROMPT.md",
		"paths":          []string{"*.txt"},
		"max_iterations": 3,
	})
	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", code)
	}

	// Give it a moment to acquire the lock
	time.Sleep(100 * time.Millisecond)

	// Second agent on same project should get 409
	code2, _ := postAgent(t, env.server.URL, map[string]interface{}{
		"project":        "test-project",
		"prompt_file":    "AGENT_PROMPT.md",
		"paths":          []string{"*.txt"},
		"max_iterations": 3,
	})
	if code2 != http.StatusConflict {
		t.Errorf("expected 409 (locked), got %d", code2)
	}

	// Also, a regular job should get 409
	code3, _ := postRun(t, env.server.URL, map[string]interface{}{
		"project":     "test-project",
		"instruction": "do stuff",
		"paths":       []string{"*.txt"},
	})
	if code3 != http.StatusConflict {
		t.Errorf("expected 409 for job during agent, got %d", code3)
	}

	// Wait for first agent to finish
	sessionID := resp["session_id"].(string)
	pollAgentUntilDone(t, env.server.URL, sessionID, 30*time.Second)
}
