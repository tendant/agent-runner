package executor

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)


// WorkspaceManager handles workspace creation and cleanup
type WorkspaceManager struct {
	TmpRoot           string
	MaxRuntimeSeconds int
}

// NewWorkspaceManager creates a new workspace manager
func NewWorkspaceManager(tmpRoot string, maxRuntimeSeconds int) *WorkspaceManager {
	return &WorkspaceManager{
		TmpRoot:           tmpRoot,
		MaxRuntimeSeconds: maxRuntimeSeconds,
	}
}

// PrepareWorkspace copies a project to an isolated workspace
func (w *WorkspaceManager) PrepareWorkspace(projectPath, jobID string) (string, error) {
	workspacePath := filepath.Join(w.TmpRoot, "job-"+jobID)

	// Ensure tmp directory exists
	if err := os.MkdirAll(w.TmpRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create tmp directory: %w", err)
	}

	// Copy project to workspace
	if err := copyDir(projectPath, workspacePath); err != nil {
		return "", fmt.Errorf("failed to copy project to workspace: %w", err)
	}

	return workspacePath, nil
}

// CleanupWorkspace removes a workspace directory
func (w *WorkspaceManager) CleanupWorkspace(workspacePath string) error {
	if workspacePath == "" {
		return nil
	}
	return os.RemoveAll(workspacePath)
}

// CleanupStaleWorkspaces removes workspaces older than max runtime
func (w *WorkspaceManager) CleanupStaleWorkspaces() error {
	entries, err := os.ReadDir(w.TmpRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // tmp directory doesn't exist yet
		}
		return fmt.Errorf("failed to read tmp directory: %w", err)
	}

	cutoff := time.Now().Add(-time.Duration(w.MaxRuntimeSeconds) * time.Second)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Only clean workspace directories; leave other subdirs (e.g. conversations/) alone.
		name := entry.Name()
		if !strings.HasPrefix(name, "session-") && !strings.HasPrefix(name, "job-") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Check if directory is older than cutoff
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(w.TmpRoot, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				// Log but continue
				fmt.Printf("Warning: failed to clean up stale workspace %s: %v\n", path, err)
			}
		}
	}

	return nil
}

// PrepareAgentWorkspace creates a workspace with workspace/ and state/ subdirectories.
// workspace/ is the agent's CWD — shared repos, _send/, _progress.json live here.
// state/ is runner-managed bookkeeping (TODO.md), invisible to the agent.
// It pre-populates shared repos from the persistent repo cache into workspace/.
// If skillsDir is set, skills are copied to .claude/skills/ and .agents/skills/ so
// Claude Code, opencode, and Codex all discover them.
// If gitHost and gitOrg are set, it configures the git remote origin for each repo.
// gitToken is injected into HTTPS remote URLs stored in each workspace repo so the
// agent's own git commands pick up credentials without extra configuration.
func (w *WorkspaceManager) PrepareAgentWorkspace(repoCacheRoot, sessionID string, sharedRepos []string, skillsDir, gitHost, gitOrg, gitToken string) (string, error) {
	workspacePath := filepath.Join(w.TmpRoot, "session-"+sessionID)

	if err := os.MkdirAll(w.TmpRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create tmp directory: %w", err)
	}

	agentDir := filepath.Join(workspacePath, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create workspace directory: %w", err)
	}

	stateDir := filepath.Join(workspacePath, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	// Pre-populate skills into both .claude/skills/ (Claude Code + opencode) and
	// .agents/skills/ (Codex + opencode) so all supported CLIs discover them.
	if skillsDir != "" {
		for _, dst := range []string{
			filepath.Join(agentDir, ".claude", "skills"),
			filepath.Join(agentDir, ".agents", "skills"),
		} {
			if err := copyDir(skillsDir, dst); err != nil {
				slog.Warn("workspace: failed to copy skills", "dst", dst, "error", err)
			}
		}
	}

	// Pre-populate shared repos from cache into workspace/
	for _, repo := range sharedRepos {
		if repo == "" {
			continue
		}
		cachedRepo := filepath.Join(repoCacheRoot, repo)
		info, err := os.Stat(cachedRepo)
		if err != nil || !info.IsDir() {
			if suggestion := suggestCachedRepo(repoCacheRoot, repo); suggestion != "" {
				slog.Warn("shared repo not found in cache; did you mean a different name?",
					"repo", repo, "suggestion", suggestion)
			} else {
				slog.Warn("shared repo not found in cache", "repo", repo, "cache", repoCacheRoot)
			}
			continue
		}

		dst := filepath.Join(agentDir, repo)
		if err := copyDir(cachedRepo, dst); err != nil {
			log.Printf("Agent workspace: warning: failed to copy shared repo %s: %v", repo, err)
			continue
		}
		log.Printf("Agent workspace: pre-populated shared repo %s from cache", repo)

		// Ensure git remote origin matches expected URL; inject token for HTTPS
		// so the agent's own git commands work without extra credential setup.
		if gitHost != "" && gitOrg != "" {
			expectedURL := fmt.Sprintf("https://%s/%s/%s.git", gitHost, gitOrg, repo)
			if gitToken != "" {
				expectedURL = fmt.Sprintf("https://oauth2:%s@%s/%s/%s.git", gitToken, gitHost, gitOrg, repo)
			}
			configureGitRemote(dst, repo, expectedURL)
		}

		// Fetch latest from origin and reset to remote HEAD so the agent
		// starts with up-to-date code, not just whatever was in the cache.
		// Non-fatal: if fetch fails (no network, bad token) the agent still
		// gets the cached state.
		if err := fetchAndResetRepo(dst, repo); err != nil {
			slog.Warn("workspace: fetch failed, using cached state", "repo", repo, "error", err)
		}
	}

	return workspacePath, nil
}

// CacheReposBack copies repos from workspace/ back to repoCacheRoot/ for future runs.
// Skips hidden and underscore-prefixed entries (_send/, _progress.json, etc.).
func (w *WorkspaceManager) CacheReposBack(workspacePath, repoCacheRoot string) {
	agentDir := filepath.Join(workspacePath, "workspace")
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		return // no workspace directory, nothing to cache
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		src := filepath.Join(agentDir, entry.Name())
		dst := filepath.Join(repoCacheRoot, entry.Name())

		if err := cacheRepoAtomic(src, dst); err != nil {
			log.Printf("Agent workspace: warning: failed to cache repo %s: %v", entry.Name(), err)
			continue
		}
		log.Printf("Agent workspace: cached repo %s back to workspaces", entry.Name())
	}
}

// cacheRepoAtomic copies src into dst without risking data loss if the copy
// fails mid-way. It uses a two-rename swap:
//
//  1. Copy src → dst.tmp   (original dst untouched if this fails)
//  2. Rename dst → dst.old (atomic; makes room for the new copy)
//  3. Rename dst.tmp → dst (atomic; new copy is now live)
//  4. Remove dst.old       (clean up; harmless if it lingers)
//
// If step 3 fails we attempt to restore dst from dst.old before returning.
func cacheRepoAtomic(src, dst string) error {
	tmp := dst + ".tmp"
	old := dst + ".old"

	// Clean up any leftovers from a previous failed run.
	os.RemoveAll(tmp)
	os.RemoveAll(old)

	// Step 1: copy into temp location — original dst is safe if this fails.
	if err := copyDir(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("copy to temp: %w", err)
	}

	// Step 2: move old cache out of the way (no-op if dst doesn't exist yet).
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("rename old cache aside: %w", err)
		}
	}

	// Step 3: move new copy into place.
	if err := os.Rename(tmp, dst); err != nil {
		// Restore old cache so we don't lose it.
		if _, statErr := os.Stat(old); statErr == nil {
			os.Rename(old, dst)
		}
		os.RemoveAll(tmp)
		return fmt.Errorf("rename new cache into place: %w", err)
	}

	// Step 4: remove old cache (best-effort; a lingering .old is harmless).
	os.RemoveAll(old)
	return nil
}

// suggestCachedRepo looks for a cached repo whose normalised name matches
// repo's normalised name. Normalisation folds to lowercase and treats '-' and
// '_' as equivalent, catching the most common typos.
// Returns the suggested name, or "" if nothing close is found.
func suggestCachedRepo(repoCacheRoot, repo string) string {
	normalise := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "-", "_"))
	}
	target := normalise(repo)
	entries, err := os.ReadDir(repoCacheRoot)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && normalise(e.Name()) == target {
			return e.Name()
		}
	}
	return ""
}

// fetchAndResetRepo fetches from origin and hard-resets the working tree to
// origin/<default-branch>. It mirrors the job handler's FetchAndReset logic
// but uses plain exec.Command so workspace.go stays free of the git package.
// The remote must already have credentials configured (via configureGitRemote).
func fetchAndResetRepo(repoPath, repoName string) error {
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	if err := run("fetch", "origin"); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}

	// Detect default branch via the symbolic ref; fall back to "main".
	branch := "main"
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = repoPath
	if out, err := cmd.Output(); err == nil {
		// output: "refs/remotes/origin/main\n"
		parts := strings.Split(strings.TrimSpace(string(out)), "/")
		if len(parts) > 0 {
			branch = parts[len(parts)-1]
		}
	}

	if err := run("reset", "--hard", "origin/"+branch); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}
	if err := run("clean", "-fdx"); err != nil {
		return fmt.Errorf("git clean: %w", err)
	}

	log.Printf("Agent workspace: fetched and reset %s to origin/%s", repoName, branch)
	return nil
}

// configureGitRemote ensures the origin remote matches the expected URL.
// It checks the current URL first and only updates if different.
func configureGitRemote(repoPath, repoName, expectedURL string) {
	// Check current remote URL
	getURL := exec.Command("git", "remote", "get-url", "origin")
	getURL.Dir = repoPath
	out, err := getURL.Output()
	if err == nil {
		currentURL := strings.TrimSpace(string(out))
		if currentURL == expectedURL {
			log.Printf("Agent workspace: git remote for %s already correct: %s", repoName, expectedURL)
			return
		}
		// Remote exists but URL differs — update it
		log.Printf("Agent workspace: updating git remote for %s: %s → %s", repoName, currentURL, expectedURL)
		setURL := exec.Command("git", "remote", "set-url", "origin", expectedURL)
		setURL.Dir = repoPath
		if err := setURL.Run(); err != nil {
			log.Printf("Agent workspace: warning: failed to update git remote for %s: %v", repoName, err)
		}
		return
	}

	// No remote — add it
	log.Printf("Agent workspace: adding git remote for %s: %s", repoName, expectedURL)
	addRemote := exec.Command("git", "remote", "add", "origin", expectedURL)
	addRemote.Dir = repoPath
	if err := addRemote.Run(); err != nil {
		log.Printf("Agent workspace: warning: failed to add git remote for %s: %v", repoName, err)
	}
}

// copyDir recursively copies a directory
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file
func copyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	// Handle symlinks
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(link, dst)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}
