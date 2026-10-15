package chatcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tmpl "github.com/agent-runner/agent-runner/internal/template"
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

func TestMemoryLogAndUndo(t *testing.T) {
	c, memDir := newMemoryCommander(t)
	os.MkdirAll(memDir, 0o755)
	if reply, _, _ := c.Handle("/memory log", nil); !strings.Contains(reply, "not a git repo") {
		t.Errorf("no repo: %q", reply)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"}} {
		exec.Command("git", append([]string{"-C", memDir}, args...)...).Run()
	}
	os.WriteFile(filepath.Join(memDir, "decisions.md"), []byte("d1\n"), 0o644)
	tmpl.CommitMemory(memDir, "init")
	os.WriteFile(filepath.Join(memDir, "decisions.md"), []byte("d1\nbad note\n"), 0o644)
	tmpl.CommitMemory(memDir, tmpl.SessionCommitMessage("agent-7", "stream", []string{"decisions.md"}))

	if reply, _, _ := c.Handle("/memory log", nil); !strings.Contains(reply, "memory: session agent-7 (stream)") {
		t.Errorf("log: %q", reply)
	}
	reply, _, _ := c.Handle("/memory undo agent-7", nil)
	if !strings.Contains(reply, "undid 1") {
		t.Errorf("undo: %q", reply)
	}
	if b, _ := os.ReadFile(filepath.Join(memDir, "decisions.md")); string(b) != "d1\n" {
		t.Errorf("not undone: %q", b)
	}
	if reply, _, _ := c.Handle("/memory undo", nil); !strings.Contains(reply, "usage") {
		t.Errorf("no id: %q", reply)
	}
}
