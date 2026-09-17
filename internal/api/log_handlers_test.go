package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/logging"
)

func writeTestLog(t *testing.T, env *testEnv, sessionID, status, errText string) {
	t.Helper()
	data := &logging.AgentLogData{
		SessionID: sessionID,
		Status:    status,
		Message:   "do the thing",
		Error:     errText,
		Iterations: []logging.AgentIterationLog{
			{Iteration: 1, Status: "error", Error: errText, Prompt: "p", Output: "partial out"},
		},
	}
	if _, err := env.handlers.runLogger.WriteAgentLog(data); err != nil {
		t.Fatal(err)
	}
}

func TestHandleListLogs(t *testing.T) {
	env := setupTestEnv(t)
	writeTestLog(t, env, "agent-aaaa-1111", "failed", "CLAUDE_ERROR: exit status 1 - boom")
	writeTestLog(t, env, "agent-bbbb-2222", "completed", "")

	req := httptest.NewRequest(http.MethodGet, "/logs?limit=10", nil)
	w := httptest.NewRecorder()
	env.handlers.HandleListLogs(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := parseJSON(w)
	logs, _ := body["logs"].([]any)
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d: %s", len(logs), w.Body.String())
	}
	var sawErr bool
	for _, l := range logs {
		m := l.(map[string]any)
		if m["session_id"] == "agent-aaaa-1111" {
			sawErr = m["status"] == "failed" && strings.Contains(m["error"].(string), "boom")
		}
	}
	if !sawErr {
		t.Errorf("failed log missing status/error: %s", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/logs?limit=0", nil)
	w = httptest.NewRecorder()
	env.handlers.HandleListLogs(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("limit=0 should be 400, got %d", w.Code)
	}
}

func TestHandleGetLog(t *testing.T) {
	env := setupTestEnv(t)
	writeTestLog(t, env, "agent-aaaa-1111", "failed", "CLAUDE_ERROR: exit status 1 - boom")
	writeTestLog(t, env, "agent-aaaa-2222", "completed", "")

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		env.handlers.HandleGetLog(w, req)
		return w
	}

	w := get("/logs/agent-aaaa-1111")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("expected markdown content type, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "boom") || !strings.Contains(w.Body.String(), "partial out") {
		t.Errorf("full log should carry error and iteration output:\n%s", w.Body.String())
	}

	if w := get("/logs/agent-aaaa-2222"); w.Code != http.StatusOK {
		t.Errorf("exact id should resolve, got %d", w.Code)
	}
	if w := get("/logs/agent-aaaa"); w.Code != http.StatusConflict {
		t.Errorf("ambiguous prefix should be 409, got %d", w.Code)
	}
	if w := get("/logs/agent-zzzz"); w.Code != http.StatusNotFound {
		t.Errorf("unknown id should be 404, got %d", w.Code)
	}
	if w := get("/logs/../etc"); w.Code != http.StatusBadRequest {
		t.Errorf("path traversal should be 400, got %d", w.Code)
	}
}

func TestHandleStartAgent_RejectsBadCallbackURL(t *testing.T) {
	env := setupTestEnv(t)
	w := postJSON(env.handlers.HandleStartAgent, map[string]any{
		"message":      "do something",
		"callback_url": "ftp://nope/hook",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad callback_url, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "callback_url") {
		t.Errorf("error should name callback_url: %s", w.Body.String())
	}
}
