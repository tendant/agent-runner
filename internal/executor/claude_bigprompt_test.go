package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturingClaude records its arguments, stdin and system prompt file, and
// answers with a stream-json result line.
const capturingClaude = `#!/bin/sh
out="$CAPTURE_DIR"
: > "$out/args"
prev=""
for a in "$@"; do
  printf '%s' "$a" | head -c 200 >> "$out/args"; printf '\n' >> "$out/args"
  if [ "$prev" = "--system-prompt-file" ]; then cp "$a" "$out/sysfile"; echo "$a" > "$out/sysfile.path"; fi
  prev="$a"
done
cat > "$out/stdin"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0}'
`

func runFakeClaude(t *testing.T, system, instruction string) (args, stdin, sysfile, sysPath string) {
	t.Helper()
	bin, capture := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(capturingClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := NewClaudeExecutor("", 0)
	e.ExtraEnv = []string{"CAPTURE_DIR=" + capture}
	if _, err := e.ExecuteWithSystemPrompt(context.Background(), t.TempDir(), system, instruction); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(capture, name))
		return string(b)
	}
	return read("args"), read("stdin"), read("sysfile"), strings.TrimSpace(read("sysfile.path"))
}

// A prompt within one argument's limit is passed as before: arguments.
func TestClaudeSmallPromptAsArguments(t *testing.T) {
	args, stdin, _, _ := runFakeClaude(t, "be brief", "fix the bug")
	if !strings.Contains(args, "--system-prompt\nbe brief") || !strings.Contains(args, "fix the bug") || stdin != "" {
		t.Errorf("args %q stdin %q", args, stdin)
	}
}

// A prompt longer than Linux allows in one argument (an inlined 900 KB
// file: "argument list too long") goes as a file and on stdin, whole.
func TestClaudeLargePromptNotAsArguments(t *testing.T) {
	system := strings.Repeat("s", 300<<10)
	instruction := "build the site\n" + strings.Repeat("<div>page</div>\n", 20<<10)
	args, stdin, sysfile, sysPath := runFakeClaude(t, system, instruction)
	if strings.Contains(args, "--system-prompt\n") || !strings.Contains(args, "--system-prompt-file") {
		t.Errorf("system prompt not passed as a file: %q", args)
	}
	if strings.Contains(args, "build the site") {
		t.Error("the instruction was passed as an argument")
	}
	if stdin != instruction {
		t.Errorf("stdin has %d bytes, want the instruction's %d", len(stdin), len(instruction))
	}
	if sysfile != system {
		t.Errorf("system prompt file has %d bytes, want %d", len(sysfile), len(system))
	}
	if _, err := os.Stat(sysPath); !os.IsNotExist(err) {
		t.Errorf("system prompt file %s left behind (%v)", sysPath, err)
	}
}
