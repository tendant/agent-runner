package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexExecutor_StreamsPromptViaStdin(t *testing.T) {
	tmpDir := t.TempDir()
	captureArgsPath := filepath.Join(tmpDir, "args.txt")
	captureStdinPath := filepath.Join(tmpDir, "stdin.txt")
	codexPath := filepath.Join(tmpDir, "codex")

	script := `#!/bin/sh
set -eu
printf '%s
' "$@" > "$CAPTURE_ARGS_PATH"
cat > "$CAPTURE_STDIN_PATH"
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$arg"
    break
  fi
  prev="$arg"
done
printf '%s' 'final response' > "$out"
`
	if err := os.WriteFile(codexPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+originalPath)
	t.Setenv("CAPTURE_ARGS_PATH", captureArgsPath)
	t.Setenv("CAPTURE_STDIN_PATH", captureStdinPath)

	executor := NewCodexExecutor("gpt-5")
	systemPrompt := "system section"
	instruction := strings.Repeat("large instruction block ", 512)

	result, err := executor.ExecuteWithSystemPrompt(context.Background(), t.TempDir(), systemPrompt, instruction)
	if err != nil {
		t.Fatalf("ExecuteWithSystemPrompt returned error: %v", err)
	}

	argsData, err := os.ReadFile(captureArgsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	argsLines := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if len(argsLines) < 6 {
		t.Fatalf("expected codex args to be captured, got %q", string(argsData))
	}
	if argsLines[0] != "exec" {
		t.Fatalf("expected first arg to be exec, got %q", argsLines[0])
	}
	if argsLines[1] != "--dangerously-bypass-approvals-and-sandbox" {
		t.Fatalf("expected sandbox bypass flag, got %q", argsLines[1])
	}
	if argsLines[2] != "-o" {
		t.Fatalf("expected -o flag, got %q", argsLines[2])
	}
	if argsLines[4] != "-m" || argsLines[5] != "gpt-5" {
		t.Fatalf("expected model args, got %q", argsLines)
	}
	if got := argsLines[len(argsLines)-1]; got != "-" {
		t.Fatalf("expected codex prompt arg to be '-', got %q", got)
	}

	stdinData, err := os.ReadFile(captureStdinPath)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	wantPrompt := systemPrompt + "\n\n" + instruction
	if got := string(stdinData); got != wantPrompt {
		t.Fatalf("unexpected stdin prompt length=%d want=%d", len(got), len(wantPrompt))
	}

	if result.Output != "final response" {
		t.Fatalf("expected output from codex output file, got %q", result.Output)
	}
}
