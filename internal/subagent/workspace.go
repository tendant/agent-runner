package subagent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ReadWorkspaceState reads the current state of the workspace for prompt injection.
// workspacePath is the agent's CWD (the workspace/ subdir). Repos live directly here.
// TODO.md is read from the sibling state/ directory.
func ReadWorkspaceState(ctx context.Context, workspacePath string) WorkspaceState {
	var state WorkspaceState

	// Read TODO.md from the sibling state/ directory
	todoPath := filepath.Join(workspacePath, "..", "state", "TODO.md")
	if data, err := os.ReadFile(todoPath); err == nil {
		state.TodoContent = strings.TrimSpace(string(data))
	}

	// List repo directories directly in workspace (skip hidden and underscore-prefixed)
	entries, err := os.ReadDir(workspacePath)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "_") {
				state.RepoNames = append(state.RepoNames, e.Name())
			}
		}
	}

	// For each repo, gather git info
	var commits []string
	var diffs []string
	for _, repoName := range state.RepoNames {
		repoDir := filepath.Join(workspacePath, repoName)

		// Recent commits
		if out := gitCmd(ctx, repoDir, "log", "--oneline", "-10"); out != "" {
			for _, line := range strings.Split(out, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					commits = append(commits, repoName+": "+line)
				}
			}
		}

		// Diff stat (uncommitted changes)
		if out := gitCmd(ctx, repoDir, "diff", "--stat"); out != "" {
			diffs = append(diffs, repoName+":\n"+out)
		}
	}

	state.RecentCommits = commits
	if len(diffs) > 0 {
		state.GitDiffStat = strings.Join(diffs, "\n")
	}

	state.Skills = readSkills(workspacePath)

	return state
}

// skillDirs are the conventional locations PrepareAgentWorkspace copies
// AGENT_SKILLS_DIR into (internal/executor/workspace.go), checked in order —
// the first one present wins, since both are populated with the same content.
var skillDirs = []string{filepath.Join(".claude", "skills"), filepath.Join(".agents", "skills")}

// readSkills reads name+description summaries from each skill's SKILL.md
// frontmatter under workspacePath, so the planner can reference available
// skills by name instead of being structurally blind to them (skills live
// under dot-directories, which the general workspace scan above skips).
func readSkills(workspacePath string) []SkillSummary {
	for _, rel := range skillDirs {
		dir := filepath.Join(workspacePath, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var skills []SkillSummary
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
			if err != nil {
				continue
			}
			name, description := parseSkillFrontmatter(string(data))
			if name == "" {
				name = e.Name()
			}
			skills = append(skills, SkillSummary{Name: name, Description: description})
		}
		if len(skills) > 0 {
			return skills
		}
	}
	return nil
}

// parseSkillFrontmatter extracts "name" and "description" from a SKILL.md's
// YAML frontmatter (--- delimited). Best-effort, single-line scalars only —
// matches the Agent Skills convention, not a general YAML parser.
func parseSkillFrontmatter(content string) (name, description string) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		idx := strings.IndexByte(trimmed, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.TrimSpace(trimmed[idx+1:])
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	return name, description
}

// gitCmd runs a git command and returns trimmed stdout, or empty string on error.
func gitCmd(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Debug("subagent: git command failed", "args", args, "dir", dir, "error", err, "stderr", strings.TrimSpace(stderr.String()))
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
