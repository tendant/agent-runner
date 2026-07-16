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
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)

	bareRepo, _ := newGitOrigin(t, baseDir, repoCacheDir, "# Test Project\n", "Initial commit")

	// Mock claude that tracks invocation count via a counter file.
	// Odd invocations (1, 3, 5) write a file; even invocations (2, 4) produce no changes.
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"

# Odd iterations produce files; even iterations are no-ops
if [ $((next %% 2)) -eq 1 ]; then
    echo "content for iteration $next" > "iteration_${next}.txt"
fi

echo '{"result":"Done","cost_usd":0.01,"duration_ms":500}'
`, counterFile))

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.MaxIterations = 5
	cfg.Agent.Paths = []string{"*.txt", "*.md"}
	cfg.Agent.Author = "test-agent"

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())

	return &e2eEnv{
		server:       ts,
		bareRepo:     bareRepo,
		repoCacheDir: repoCacheDir,
		logsDir:      logsDir,
		tmpDir:       tmpDir,
		mockBinDir:   mockBinDir,
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
	return pollAgentUntilDoneInterval(t, serverURL, sessionID, timeout, 300*time.Millisecond)
}

func pollAgentUntilDoneInterval(t *testing.T, serverURL, sessionID string, timeout, interval time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, result := getAgent(t, serverURL, sessionID)
		status, _ := result["status"].(string)
		if status == "completed" || status == "failed" {
			return result
		}
		time.Sleep(interval)
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
	entries, err := os.ReadDir(env.logsDir)
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
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)
	newGitOrigin(t, baseDir, repoCacheDir, "", "")

	// Slow mock claude
	writeMockClaude(t, mockBinDir, `#!/bin/bash
sleep 1
count=$(ls -1 iteration_*.txt 2>/dev/null | wc -l | tr -d ' ')
next=$((count + 1))
echo "content" > "iteration_${next}.txt"
echo '{"result":"Done"}'
`)

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.Paths = []string{"*.txt", "*.md"}

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
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)
	memoryDir := filepath.Join(baseDir, "memory")
	os.MkdirAll(memoryDir, 0755)

	newGitOrigin(t, baseDir, repoCacheDir, "", "")

	// Capture file records system prompt args for verification
	captureFile := filepath.Join(baseDir, "captured-args.txt")

	// Mock claude that captures --system-prompt and creates a file
	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
echo "$@" >> %s
echo "content" > "output.txt"
echo '{"result":"Done"}'
`, captureFile))

	// Write prompt.md directly into the memory dir so resolvePrompt picks it up.
	if err := os.WriteFile(filepath.Join(memoryDir, "prompt.md"), []byte("You are a helpful assistant.\n\nTask: {{MESSAGE}}"), 0644); err != nil {
		t.Fatalf("write prompt.md error: %v", err)
	}

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MemoryDir = memoryDir
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.MaxIterations = 1
	cfg.Agent.Paths = []string{"*.txt", "*.md"}

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

	// Verify the seeded prompt content reached mock claude via --system-prompt
	captured, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("failed to read captured args: %v", err)
	}
	args := string(captured)
	if !strings.Contains(args, "You are a helpful assistant.") {
		t.Errorf("expected prompt content in claude args, got:\n%s", args)
	}
	if !strings.Contains(args, "Fix the login bug") {
		t.Errorf("expected {{MESSAGE}} substituted in prompt, got:\n%s", args)
	}
}

func TestE2E_AgentNoProjectNoDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()
	repoCacheDir, logsDir, _, mockBinDir := newE2EDirs(t, baseDir)

	// Use a separate temp dir for workspaces so background goroutines
	// from executeAgent don't race with t.TempDir() cleanup.
	tmpDir := newManagedTempDir(t, "agent-e2e-noproject-*")

	// The server now preflights the CLI backend binary before accepting a
	// session — provide a no-op mock so this test (which is about project
	// resolution, not CLI behavior) doesn't depend on a real CLI being
	// installed on the host.
	writeMockClaude(t, mockBinDir, "#!/bin/bash\necho '{\"result\":\"Done\"}'\n")

	cfg := &config.Config{
		RepoCacheRoot:     repoCacheDir,
		LogsRoot:          logsDir,
		TmpRoot:           tmpDir,
		AllowedProjects:   []string{},
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
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)
	newGitOrigin(t, baseDir, repoCacheDir, "", "")

	// Mock claude that returns plan JSON on invocation 2, normal execution afterwards.
	// Invocation 1 is POST /agent's analyzer routing call (ask/plan/execute) —
	// it falls back to this same mock CLI since no Analyzer credentials are
	// configured, and its non-JSON-router output is harmlessly treated as
	// "execute" by Analyze's parse-failure fallback.
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"

if [ "$next" -eq 2 ]; then
    # Second invocation is the planner — return plan JSON
    echo '{"result":"{\"summary\":\"Test plan\",\"steps\":[{\"id\":\"1\",\"description\":\"Create file\",\"done\":false},{\"id\":\"2\",\"description\":\"Add content\",\"done\":false}],\"approach\":\"Direct implementation\"}"}'
else
    # Analyzer call and subsequent invocations are iteration work
    echo "content for iteration $next" > "work_${next}.txt"
    echo '{"result":"Done","cost_usd":0.01,"duration_ms":500}'
fi
`, counterFile))

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.MaxIterations = 3
	cfg.Agent.Paths = []string{"*.txt", "*.md"}
	cfg.Agent.Author = "test-agent"
	cfg.Agent.PlannerEnabled = true

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
		entries, err := os.ReadDir(logsDir)
		if err != nil {
			t.Fatalf("failed to read runs dir: %v", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				data, _ := os.ReadFile(filepath.Join(logsDir, entry.Name()))
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

func TestE2E_AgentWithPlannerProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)
	newGitOrigin(t, baseDir, repoCacheDir, "", "")

	// Mock claude:
	// Invocation 1 is POST /agent's analyzer routing call (falls back to this
	// same mock CLI, running in /tmp — see TestE2E_AgentWithPlanner for why).
	// Invocation 2 (planner): returns plan with 3 steps
	// Invocation 3+: writes _progress.json with cumulative step IDs and creates work files
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"

if [ "$next" -eq 2 ]; then
    # Planner: return plan with 3 steps
    echo '{"result":"{\"summary\":\"Build feature\",\"steps\":[{\"id\":\"1\",\"description\":\"Create module\",\"done\":false},{\"id\":\"2\",\"description\":\"Add tests\",\"done\":false},{\"id\":\"3\",\"description\":\"Update docs\",\"done\":false}],\"approach\":\"Incremental\"}"}'
elif [ "$next" -eq 3 ]; then
    echo '{"completed_steps":["1"]}' > _progress.json
    echo "module code" > module.txt
    echo '{"result":"Done step 1"}'
elif [ "$next" -eq 4 ]; then
    echo '{"completed_steps":["1","2"]}' > _progress.json
    echo "test code" > tests.txt
    echo '{"result":"Done step 2"}'
else
    echo '{"completed_steps":["1","2","3"]}' > _progress.json
    echo "docs" > docs.txt
    echo '{"result":"Done step 3"}'
fi
`, counterFile))

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.MaxIterations = 3
	cfg.Agent.Paths = []string{"*.txt", "*.md", "*.json"}
	cfg.Agent.Author = "test-agent"
	cfg.Agent.PlannerEnabled = true

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Build the feature",
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

	// Verify plan is present
	if _, hasPlan := result["plan"]; !hasPlan {
		t.Error("expected plan in response")
	}

	// Verify completed_steps contains the final step IDs
	completedRaw, hasSteps := result["completed_steps"]
	if !hasSteps {
		t.Fatal("expected completed_steps in response")
	}
	completedSlice, ok := completedRaw.([]interface{})
	if !ok {
		t.Fatalf("expected completed_steps to be array, got %T", completedRaw)
	}

	// Should have all 3 steps completed
	if len(completedSlice) != 3 {
		t.Fatalf("expected 3 completed steps, got %d: %v", len(completedSlice), completedSlice)
	}

	stepIDs := make(map[string]bool)
	for _, s := range completedSlice {
		stepIDs[s.(string)] = true
	}
	for _, id := range []string{"1", "2", "3"} {
		if !stepIDs[id] {
			t.Errorf("expected step %s in completed_steps", id)
		}
	}

	// Verify iterations ran
	iterations, ok := result["iterations"].([]interface{})
	if !ok {
		t.Fatal("expected iterations in response")
	}
	if len(iterations) != 3 {
		t.Fatalf("expected 3 iterations, got %d", len(iterations))
	}
}

func TestE2E_AgentQueueing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	baseDir := t.TempDir()
	repoCacheDir, logsDir, tmpDir, mockBinDir := newE2EDirs(t, baseDir)
	newGitOrigin(t, baseDir, repoCacheDir, "", "")

	// Slow mock claude — sleeps 2s per iteration so the first agent holds the queue
	counterFile := filepath.Join(baseDir, "counter")
	os.WriteFile(counterFile, []byte("0"), 0644)

	writeMockClaude(t, mockBinDir, fmt.Sprintf(`#!/bin/bash
COUNTER_FILE="%s"
count=$(cat "$COUNTER_FILE")
next=$((count + 1))
echo "$next" > "$COUNTER_FILE"
sleep 2
echo "content for $next" > "output_${next}.txt"
echo '{"result":"Done"}'
`, counterFile))

	cfg := baseE2EConfig(repoCacheDir, logsDir, tmpDir)
	cfg.MaxRuntimeSeconds = 60
	cfg.Agent.MaxIterations = 3
	cfg.Agent.Paths = []string{"*.txt", "*.md"}
	cfg.Agent.Author = "test-agent"

	srv := api.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Start first agent — should get 202 with status queued
	code1, resp1 := postAgent(t, ts.URL, map[string]interface{}{
		"message": "First task",
	})
	if code1 != http.StatusAccepted {
		t.Fatalf("expected 202 for first agent, got %d: %v", code1, resp1)
	}
	sessionID1, _ := resp1["session_id"].(string)
	if resp1["status"] != "queued" {
		t.Errorf("expected first agent status 'queued', got %v", resp1["status"])
	}

	// Give first agent time to start running (dispatch picks it up)
	time.Sleep(500 * time.Millisecond)

	// Start second agent while first is running — should also get 202 queued
	code2, resp2 := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Second task",
	})
	if code2 != http.StatusAccepted {
		t.Fatalf("expected 202 for second agent, got %d: %v", code2, resp2)
	}
	sessionID2, _ := resp2["session_id"].(string)
	if resp2["status"] != "queued" {
		t.Errorf("expected second agent status 'queued', got %v", resp2["status"])
	}

	// Verify second agent is queued while first is running
	_, pollResp2 := getAgent(t, ts.URL, sessionID2)
	status2, _ := pollResp2["status"].(string)
	if status2 != "queued" {
		t.Logf("second agent status: %s (may have already started if first finished fast)", status2)
	}

	// Both should complete successfully (second runs after first)
	result1 := pollAgentUntilDone(t, ts.URL, sessionID1, 60*time.Second)
	status1, _ := result1["status"].(string)
	if status1 != "completed" {
		t.Fatalf("expected first agent to complete, got %s: error=%v", status1, result1["error"])
	}

	result2 := pollAgentUntilDone(t, ts.URL, sessionID2, 60*time.Second)
	finalStatus2, _ := result2["status"].(string)
	if finalStatus2 != "completed" {
		t.Fatalf("expected second agent to complete, got %s: error=%v", finalStatus2, result2["error"])
	}
}
