package subagent

import (
	"bytes"
	"context"
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

	return state
}

// gitCmd runs a git command and returns trimmed stdout, or empty string on error.
func gitCmd(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
