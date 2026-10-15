package clisetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The earliest default agent.md, as /bootstrap wrote it.
const firstAgentMD = "# Agent\n\nYou are an autonomous software development agent. Your working directory is `workspace/`.\n\n## Core Principles\n\n- Complete tasks thoroughly before reporting done\n- Commit all changes with descriptive commit messages\n- Prefer making progress over waiting for perfect information\n- Ask for clarification only when the task is genuinely ambiguous\n\n## When Done\n\nReport what you did in 2-3 sentences. List files changed and key commands run.\n"

func TestUpgradeDefaultPrompts(t *testing.T) {
	dir := t.TempDir()
	agent, prompt := filepath.Join(dir, "agent.md"), filepath.Join(dir, "prompt.md")

	// An unedited old default, saved with CRLF and trailing spaces by an editor.
	os.WriteFile(agent, []byte(strings.ReplaceAll(firstAgentMD, "\n", "  \r\n")), 0o644)
	os.WriteFile(prompt, []byte(DefaultPromptMD), 0o644) // already current
	done, err := UpgradeDefaultPrompts(agent, prompt)
	if err != nil || len(done) != 1 || done[0] != agent {
		t.Fatalf("upgraded %v, %v", done, err)
	}
	if b, _ := os.ReadFile(agent); string(b) != DefaultAgentMD {
		t.Errorf("agent.md not the current default:\n%s", b)
	}

	// Edited files are the operator's: kept.
	edited := firstAgentMD + "\n- Always answer in Chinese\n"
	os.WriteFile(agent, []byte(edited), 0o644)
	if done, _ := UpgradeDefaultPrompts(agent, prompt); len(done) != 0 {
		t.Errorf("an edited file was replaced: %v", done)
	}
	if b, _ := os.ReadFile(agent); string(b) != edited {
		t.Error("edited agent.md changed")
	}

	// Missing files are left for /bootstrap.
	if done, err := UpgradeDefaultPrompts(filepath.Join(dir, "none.md"), filepath.Join(dir, "none2.md")); err != nil || len(done) != 0 {
		t.Errorf("missing files: %v %v", done, err)
	}
}

func TestPromptCheck(t *testing.T) {
	if p := PromptCheck(DefaultAgentMD + DefaultPromptMD); len(p) != 0 {
		t.Errorf("the current defaults must pass: %v", p)
	}
	old := "- Commit all changes with descriptive commit messages and push to origin\n" +
		"API keys and credentials (ANTHROPIC_API_KEY, ...) are available directly\nas environment variables\n" +
		"Call {{RUNNER_URL}} with key {{API_KEY}}\n"
	got := strings.Join(PromptCheck(old), "\n")
	for _, want := range []string{"tells the agent to push", "API keys are in the environment", "runner's API key"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"- Commit all changes with descriptive commit messages and push to origin"`) {
		t.Errorf("each problem should quote its line:\n%s", got)
	}
}
