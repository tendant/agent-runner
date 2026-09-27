package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockLLM is an OpenAI-compatible streaming chat endpoint: it replies "OK"
// when the latest message mentions PELICAN, "SAW-CODEWORD" when only earlier
// messages do (the conversation was restored), else "NO-CODEWORD".
func mockLLM(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reply := "NO-CODEWORD"
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		json.Unmarshal(body, &req)
		n := len(req.Messages)
		if n > 0 && strings.Contains(string(body), "PELICAN") && !strings.Contains(string(req.Messages[n-1]), "PELICAN") {
			reply = "SAW-CODEWORD"
		} else if n > 0 && strings.Contains(string(req.Messages[n-1]), "PELICAN") {
			reply = "OK"
		}
		t.Logf("mock: %d messages -> %s", n, reply)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "mock", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": reply}}}})
		chunk(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "mock", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestLiveSessionResume runs the real pi binary (PI_LIVE=1) against mockLLM
// to check that --session-dir/--session-id restore a conversation in a new
// process. Re-run it after upgrading pi.
func TestLiveSessionResume(t *testing.T) {
	if os.Getenv("PI_LIVE") == "" {
		t.Skip()
	}
	srv := mockLLM(t)
	defer srv.Close()
	agentDir := t.TempDir()
	cfg := fmt.Sprintf(`{"providers":{"mock":{"baseUrl":%q,"api":"openai-completions","apiKey":"x","compat":{"supportsDeveloperRole":false,"supportsReasoningEffort":false},"models":[{"id":"mock-1"}]}}}`, srv.URL+"/v1")
	os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(cfg), 0o644)
	env := []string{"PI_CODING_AGENT_DIR=" + agentDir, "PI_OFFLINE=1", "PI_TELEMETRY=0"}

	dir := t.TempDir()
	ws := t.TempDir()
	id := "0d3c9a52-6f0e-4a8b-9d1e-2b7c4f5a6e01"
	run := func(msg string, sessDir, sessID string) string {
		c, err := Start(Options{Binary: "pi", Provider: "mock", Model: "mock-1", Workspace: ws, Env: env, SessionDir: sessDir, SessionID: sessID})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		r, err := c.Prompt(ctx, msg)
		if err != nil {
			t.Fatalf("prompt: %v\n%s", err, c.StderrTail())
		}
		return r.Text
	}
	t.Log("turn1:", run("Remember the codeword PELICAN-42.", dir, id))
	out := run("What was the codeword?", dir, id)
	t.Log("turn2 (resumed):", out)
	fresh := run("What was the codeword?", "", "")
	t.Log("no-session:", fresh)
	if !strings.Contains(out, "SAW-CODEWORD") || !strings.Contains(fresh, "NO-CODEWORD") {
		t.Fatal("pi did not restore the conversation")
	}
}
