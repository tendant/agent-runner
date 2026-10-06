package executor

import (
	"os"
	"strings"
	"testing"
)

// Agent-CLI executors must start processes only via a Launcher so a sandbox
// cannot be bypassed by a new direct exec.Command. Git helpers in workspace.go
// are runner-side and trusted.
func TestAgentCLIsNeverExecDirectly(t *testing.T) {
	for _, f := range []string{"executor.go", "codex.go", "opencode.go", "pi_backend.go", "claude_stream.go", "conversation.go", "session.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "exec.Command") {
			t.Errorf("%s calls exec.Command directly; use a Launcher", f)
		}
	}
}
