package e2e

// Integration tests that run against a real CLI backend.
//
// Set INTEGRATION_CLI to the binary name (claude / codex / opencode) to enable.
// The matching API key env var must also be present (ANTHROPIC_API_KEY,
// OPENAI_API_KEY, etc.).
//
// Example:
//   INTEGRATION_CLI=codex OPENAI_API_KEY=sk-... go test -v -run Integration ./e2e/
//
// Override provider/model:
//   INTEGRATION_CLI=codex INTEGRATION_MODEL=gpt-4o OPENAI_API_KEY=sk-... go test ...

import (
	"bytes"
	"encoding/json"
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

// integrationCLI returns the CLI name from INTEGRATION_CLI, or skips the test.
func integrationCLI(t *testing.T) string {
	t.Helper()
	cli := os.Getenv("INTEGRATION_CLI")
	if cli == "" {
		t.Skip("set INTEGRATION_CLI=<claude|codex|opencode> to run integration tests")
	}
	return cli
}

// cliDefaults returns sensible provider/model defaults for each CLI.
// opencode defaults match the server's own defaults (deepseek).
func cliDefaults(cli string) (provider, model string) {
	switch cli {
	case "claude":
		return "", "claude-haiku-4-5-20251001"
	case "codex":
		return "", "gpt-4o-mini"
	default: // opencode
		return "deepseek", "deepseek-chat"
	}
}

// setupIntegrationEnv builds a real git repo workspace and starts the server
// configured for the given CLI. No mock binary is injected — the real binary
// on PATH is used.
func setupIntegrationEnv(t *testing.T, cli string) (ts *httptest.Server, logsDir string) {
	t.Helper()
	baseDir := t.TempDir()

	bareRepo := filepath.Join(baseDir, "origin.git")
	repoCacheDir := filepath.Join(baseDir, "repo-cache")
	logsDir = filepath.Join(baseDir, "logs")
	tmpDir := filepath.Join(baseDir, "tmp")

	os.MkdirAll(repoCacheDir, 0755)
	os.MkdirAll(logsDir, 0755)
	os.MkdirAll(tmpDir, 0755)

	runCmd(t, "", "git", "init", "--bare", bareRepo)
	projectPath := filepath.Join(repoCacheDir, "test-project")
	runCmd(t, "", "git", "clone", bareRepo, projectPath)
	runCmd(t, projectPath, "git", "config", "user.email", "integration@test.com")
	runCmd(t, projectPath, "git", "config", "user.name", "Integration Test")
	os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("# Integration Test Project\n"), 0644)
	runCmd(t, projectPath, "git", "add", "-A")
	runCmd(t, projectPath, "git", "commit", "-m", "Initial commit")
	runCmd(t, projectPath, "git", "push", "origin", "HEAD")

	provider, model := cliDefaults(cli)
	if p := os.Getenv("INTEGRATION_PROVIDER"); p != "" {
		provider = p
	}
	if m := os.Getenv("INTEGRATION_MODEL"); m != "" {
		model = m
	}

	cfg := &config.Config{
		RepoCacheRoot:            repoCacheDir,
		LogsRoot:                 logsDir,
		TmpRoot:                  tmpDir,
		AllowedProjects:          []string{},
		MaxRuntimeSeconds:        600,
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
			CLI:               cli,
			Provider:          provider,
			Model:             model,
			ReasoningProvider: provider,
			ReasoningModel:    model,
			MaxIterations:     5,
			MaxTotalSeconds:   300,
			MaxIterationSeconds: 120,
			Paths:             []string{"*.txt", "*.md"},
			Author:            "integration-test",
			CommitPrefix:      "[integration]",
		},
		JobRetentionSeconds:     3600,
		StartupCleanupStaleJobs: false,
	}

	srv := api.NewServer(cfg)
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, logsDir
}

// TestIntegration_Bootstrap verifies /bootstrap reports the real CLI version.
func TestIntegration_Bootstrap(t *testing.T) {
	cli := integrationCLI(t)
	ts, _ := setupIntegrationEnv(t, cli)

	resp, err := http.Post(ts.URL+"/bootstrap", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST /bootstrap: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)

	installed, _ := body["cli_installed"].(bool)
	version, _ := body["cli_version"].(string)

	t.Logf("cli=%s cli_installed=%v cli_version=%q", cli, installed, version)

	if !installed {
		t.Errorf("%s not found on PATH — install it first", cli)
	}
	if version == "" {
		t.Errorf("expected cli_version to be non-empty for %s", cli)
	}
}

// TestIntegration_AgentWritesFile verifies that the real CLI can complete a
// simple file-creation task through the agent loop end-to-end.
func TestIntegration_AgentWritesFile(t *testing.T) {
	cli := integrationCLI(t)
	ts, logsDir := setupIntegrationEnv(t, cli)

	code, resp := postAgent(t, ts.URL, map[string]interface{}{
		"message": "Create a file named hello.txt containing exactly the text: hello world",
	})
	if code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %v", code, resp)
	}

	sessionID, ok := resp["session_id"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("expected session_id in response, got: %v", resp)
	}
	t.Logf("session %s started with cli=%s", sessionID, cli)

	result := pollAgentUntilDone(t, ts.URL, sessionID, 5*time.Minute)

	status, _ := result["status"].(string)
	errMsg, _ := result["error"].(string)
	iterations, _ := result["successful_iterations"].(float64)
	cost, _ := result["total_cost_usd"].(float64)

	// Log audit file for manual inspection.
	if entries, err := os.ReadDir(logsDir); err == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), sessionID[:8]) {
				t.Logf("audit log: %s", filepath.Join(logsDir, e.Name()))
			}
		}
	}

	t.Logf("status=%s iterations=%.0f cost=$%.4f error=%q", status, iterations, cost, errMsg)

	if status != "completed" {
		t.Fatalf("agent did not complete: status=%s error=%s", status, errMsg)
	}
}
