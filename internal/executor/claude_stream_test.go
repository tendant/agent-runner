package executor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sampleStream mirrors what `claude --print --output-format stream-json
// --verbose` emits for a two-tool turn, trimmed to the fields we read.
const sampleStream = `{"type":"system","subtype":"init","cwd":"/w","session_id":"s1","tools":["Bash","Read"]}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me run the tests first."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"go test ./...\necho done","description":"run tests"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok  pkg 0.1s","is_error":false}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_2","name":"Read","input":{"file_path":"/w/main.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","content":[{"type":"text","text":"File does not exist.\nmore"}],"is_error":true}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All good."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"All good.","total_cost_usd":0.0421,"duration_ms":5120,"permission_denials":[],"api_error_status":null}
`

func TestStreamParser_EventsAndResult(t *testing.T) {
	var events []string
	p := newStreamParser(func(k EventKind, text string) { events = append(events, string(k)+"|"+text) })
	if err := p.consume(strings.NewReader(sampleStream)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"text|Let me run the tests first.",
		"tool_start|Bash: go test ./... …",
		"tool_end|Bash ok",
		"tool_start|Read: /w/main.go",
		"tool_end|Read error: File does not exist. …",
		"text|All good.",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Errorf("events mismatch\n got: %q\nwant: %q", events, want)
	}
	res, ok := p.result()
	if !ok {
		t.Fatal("expected a result line")
	}
	if res.Output != "All good." || res.CostUSD != 0.0421 || res.DurationMS != 5120 || res.Error != nil {
		t.Errorf("unexpected result: %+v", res)
	}
	if !strings.Contains(p.transcript.String(), "[tool_start] Bash") || !strings.Contains(p.transcript.String(), "[result] success") {
		t.Errorf("transcript should carry events and result:\n%s", p.transcript.String())
	}
}

func TestStreamParser_ErrorResult(t *testing.T) {
	p := newStreamParser(nil)
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key","total_cost_usd":0,"duration_ms":10,"api_error_status":401}` + "\n"
	if err := p.consume(strings.NewReader(line)); err != nil {
		t.Fatal(err)
	}
	res, ok := p.result()
	if !ok || res.Error == nil {
		t.Fatalf("expected error result, got ok=%v res=%+v", ok, res)
	}
	msg := res.Error.Error()
	for _, want := range []string{"CLAUDE_ERROR", "Invalid API key", "401", "error_during_execution"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

func TestStreamParser_MaxTurnsAndDenials(t *testing.T) {
	var warnings []string
	p := newStreamParser(func(k EventKind, text string) {
		if k == EventWarning {
			warnings = append(warnings, text)
		}
	})
	line := `{"type":"result","subtype":"error_max_turns","is_error":false,"result":"","num_turns":5,"permission_denials":[{"tool_name":"Bash"}]}` + "\n"
	p.consume(strings.NewReader(line))
	res, _ := p.result()
	if res.Error == nil || !strings.Contains(res.Error.Error(), "error_max_turns") {
		t.Errorf("non-success subtype should be an error: %v", res.Error)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Bash") {
		t.Errorf("expected a permission-denied warning, got %v", warnings)
	}
}

func TestStreamParser_OlderCostField(t *testing.T) {
	p := newStreamParser(nil)
	p.consume(strings.NewReader(`{"type":"result","subtype":"success","result":"x","cost_usd":0.5}` + "\n"))
	res, _ := p.result()
	if res.CostUSD != 0.5 {
		t.Errorf("cost_usd fallback: got %v", res.CostUSD)
	}
}

func TestStreamParser_LegacyJSONObject(t *testing.T) {
	p := newStreamParser(nil)
	p.consume(strings.NewReader(`{"result":"Done","cost_usd":0.01,"duration_ms":500}` + "\n"))
	res, ok := p.result()
	if !ok || res.Output != "Done" || res.CostUSD != 0.01 || res.Error != nil {
		t.Errorf("legacy object should be accepted as result: ok=%v res=%+v", ok, res)
	}

	p = newStreamParser(nil)
	p.consume(strings.NewReader(`{"error":"rate limited"}` + "\n"))
	res, ok = p.result()
	if !ok || res.Error == nil || !strings.Contains(res.Error.Error(), "rate limited") {
		t.Errorf("legacy error object should be an error result: ok=%v res=%+v", ok, res)
	}
}

func TestStreamParser_IgnoresGarbage(t *testing.T) {
	p := newStreamParser(nil)
	p.consume(strings.NewReader("warning: something\n{not json\n\n"))
	if _, ok := p.result(); ok || p.sawLine {
		t.Error("garbage should produce neither result nor sawLine")
	}
}

func TestSummarizeToolUse(t *testing.T) {
	cases := map[string]string{
		`Bash|{"command":"ls -la"}`:                          "Bash: ls -la",
		`Edit|{"file_path":"a.go","old_string":"x"}`:         "Edit: a.go",
		`Grep|{"pattern":"TODO","path":"internal"}`:          "Grep: TODO in internal",
		`Task|{"description":"find callers","prompt":"..."}`: "Task: find callers",
		`WebFetch|{"url":"https://x.y"}`:                     "WebFetch: https://x.y",
		`mcp__foo__bar|{"query":"q"}`:                        "mcp__foo__bar: q",
		`mcp__foo__baz|{"weird":1}`:                          `mcp__foo__baz: {"weird":1}`,
		`Nothing|{}`:                                         "Nothing",
	}
	for in, want := range cases {
		name, input, _ := strings.Cut(in, "|")
		if got := summarizeToolUse(name, []byte(input)); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

// fakeClaude writes a shell script named "claude" on PATH that prints the
// given stdout, the given stderr, and exits with code.
func fakeClaude(t *testing.T, stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOS'\n" + stdout + "EOS\n"
	if stderr != "" {
		script += "echo '" + stderr + "' >&2\n"
	}
	script += "exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClaudeExecutor_StreamingEndToEnd(t *testing.T) {
	fakeClaude(t, sampleStream, "", 0)
	var kinds []EventKind
	res, err := NewClaudeExecutor("", 0).ExecuteStreaming(context.Background(), t.TempDir(), "sys", "do it",
		func(k EventKind, _ string) { kinds = append(kinds, k) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Output != "All good." || res.CostUSD != 0.0421 {
		t.Errorf("result: %+v", res)
	}
	if len(kinds) != 6 {
		t.Errorf("expected 6 events, got %v", kinds)
	}
	if !strings.Contains(res.RawOutput, "[tool_start] Read: /w/main.go") {
		t.Errorf("RawOutput should be the transcript:\n%s", res.RawOutput)
	}
}

func TestClaudeExecutor_NonZeroExitUsesResultLine(t *testing.T) {
	fakeClaude(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key","api_error_status":401}`+"\n", "auth failed", 1)
	res, err := NewClaudeExecutor("", 0).ExecuteStreaming(context.Background(), t.TempDir(), "", "x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Invalid API key") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error should come from the result line: %v", err)
	}
	if !strings.Contains(res.RawOutput, "auth failed") {
		t.Errorf("stderr should be preserved in RawOutput: %q", res.RawOutput)
	}
}

func TestClaudeExecutor_NonZeroExitWithoutResultLineUsesStderr(t *testing.T) {
	fakeClaude(t, "", "boom: no such model", 2)
	_, err := NewClaudeExecutor("", 0).ExecuteStreaming(context.Background(), t.TempDir(), "", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 2") || !strings.Contains(err.Error(), "no such model") {
		t.Errorf("expected exit code and stderr in error, got %v", err)
	}
}

func TestClaudeExecutor_PlainOutputFallback(t *testing.T) {
	fakeClaude(t, "just some text\n", "", 0)
	res, err := NewClaudeExecutor("", 0).ExecuteStreaming(context.Background(), t.TempDir(), "", "x", nil)
	if err != nil || res.Output != "just some text" {
		t.Errorf("non-stream output should pass through: res=%+v err=%v", res, err)
	}
}

func TestClaudeExecutor_StreamCutOffIsError(t *testing.T) {
	fakeClaude(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n", "", 0)
	_, err := NewClaudeExecutor("", 0).ExecuteStreaming(context.Background(), t.TempDir(), "", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "without a result line") {
		t.Errorf("expected truncated-stream error, got %v", err)
	}
}
