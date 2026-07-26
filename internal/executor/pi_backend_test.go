package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePiScript is a minimal pi RPC responder: captures argv and every stdin
// line, then settles each prompt with a fixed assistant message.
const fakePiScript = `#!/bin/sh
printf '%s\n' "$@" > "$CAPTURE_ARGS_PATH"
[ -n "$CAPTURE_PID_PATH" ] && echo $$ >> "$CAPTURE_PID_PATH"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$CAPTURE_STDIN_PATH"
  case "$line" in
  *'"type":"prompt"'*)
    id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    printf '{"type":"response","id":"%s","command":"prompt","success":true}\n' "$id"
    printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"pi says hi"}]},"usage":{"cost":0.02}}\n'
    printf '{"type":"agent_settled"}\n'
    ;;
  *'"type":"get_last_assistant_text"'*)
    id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    printf '{"type":"response","id":"%s","command":"get_last_assistant_text","success":true,"data":{"text":"pi says hi"}}\n' "$id"
    ;;
  esac
done
`

func TestPiExecutor(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "pi")
	if err := os.WriteFile(scriptPath, []byte(fakePiScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	argsPath := filepath.Join(tmpDir, "args.txt")
	stdinPath := filepath.Join(tmpDir, "stdin.txt")
	t.Setenv("CAPTURE_ARGS_PATH", argsPath)
	t.Setenv("CAPTURE_STDIN_PATH", stdinPath)

	e := NewPiExecutor("anthropic", "claude-sonnet-5")
	result, err := e.ExecuteWithSystemPrompt(context.Background(), tmpDir, "system says", "do the thing")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Output != "pi says hi" {
		t.Errorf("output = %q, want %q", result.Output, "pi says hi")
	}
	if result.CostUSD != 0.02 {
		t.Errorf("cost = %v, want 0.02", result.CostUSD)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "--mode\nrpc\n--no-session\n--provider\nanthropic\n--model\nclaude-sonnet-5\n"
	if string(args) != wantArgs {
		t.Errorf("args:\n got %q\nwant %q", args, wantArgs)
	}

	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	// The system prompt is prepended to the instruction in the prompt message.
	if !strings.Contains(string(stdin), `system says\n\ndo the thing`) {
		t.Errorf("prompt message missing concatenated system prompt:\n%s", stdin)
	}
}

// A PiBackend session keeps one process alive across prompts.
func TestPiBackendSessionReusesProcess(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "pi")
	if err := os.WriteFile(scriptPath, []byte(fakePiScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_ARGS_PATH", filepath.Join(tmpDir, "args.txt"))
	t.Setenv("CAPTURE_STDIN_PATH", filepath.Join(tmpDir, "stdin.txt"))
	pidPath := filepath.Join(tmpDir, "pids.txt")
	t.Setenv("CAPTURE_PID_PATH", pidPath)

	b := NewPiBackend("", "")
	sess, err := b.Start(context.Background(), tmpDir, SessionOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sess.Close()
	if !b.Persistent() {
		t.Error("pi backend must report persistent")
	}

	for i := 0; i < 2; i++ {
		res, err := sess.Prompt(context.Background(), PromptRequest{Message: "go"})
		if err != nil {
			t.Fatalf("prompt %d: %v", i+1, err)
		}
		if res.Output != "pi says hi" {
			t.Errorf("prompt %d output = %q", i+1, res.Output)
		}
	}

	pids, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(string(pids))); n != 1 {
		t.Errorf("pi started %d processes across 2 prompts, want 1", n)
	}
}

func TestNewExecutorPi(t *testing.T) {
	e := NewExecutor("pi", "openai", "gpt-5", 30)
	pe, ok := e.(*PiExecutor)
	if !ok {
		t.Fatalf("NewExecutor(pi) = %T, want *PiExecutor", e)
	}
	if pe.Provider != "openai" || pe.Model != "gpt-5" {
		t.Errorf("provider/model = %s/%s", pe.Provider, pe.Model)
	}
}
