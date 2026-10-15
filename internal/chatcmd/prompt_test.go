package chatcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptCommand(t *testing.T) {
	c, memDir := newMemoryCommander(t)
	os.MkdirAll(memDir, 0o755)
	os.WriteFile(filepath.Join(memDir, "agent.md"), []byte("# Agent\n- Commit all changes with descriptive commit messages and push to origin\n"), 0o644)

	reply, _, ok := c.Handle("/prompt show agent", nil)
	if !ok || !strings.Contains(reply, "push to origin") || strings.Contains(reply, "workflow") {
		t.Errorf("show agent: %q", reply)
	}
	if reply, _, _ := c.Handle("/prompt show", nil); !strings.Contains(reply, "workflow") || !strings.Contains(reply, "missing") {
		t.Errorf("show: %q", reply)
	}
	if reply, _, _ := c.Handle("/prompt check", nil); !strings.Contains(reply, "tells the agent to push") {
		t.Errorf("check: %q", reply)
	}
	if reply, _, _ := c.Handle("/prompt preview fix the bug", nil); !strings.Contains(reply, "## Current Request") || strings.Contains(reply, "fix the bug") {
		t.Errorf("preview should outline, not print: %q", reply)
	}
	if reply, _, _ := c.Handle("/prompt preview full fix the bug", nil); !strings.Contains(reply, "fix the bug") {
		t.Errorf("preview full: %q", reply)
	}
	if reply, _, _ := c.Handle("/prompt nope", nil); !strings.HasPrefix(reply, "error: usage") {
		t.Errorf("unknown: %q", reply)
	}
}
