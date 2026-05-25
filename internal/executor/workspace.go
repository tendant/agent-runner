package executor

import (
	"fmt"
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
				slog.Warn("failed to clean up stale workspace", "path", path, "error", err)
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
// envFile is copied into the workspace as .env so the agent can read project credentials.
// Returns the workspace path, a list of repos that were listed in sharedRepos but
// missing from the cache, and any setup error.
func (w *WorkspaceManager) PrepareAgentWorkspace(repoCacheRoot, sessionID string, sharedRepos []string, skillsDir, gitHost, gitOrg, gitToken, envFile string) (string, []string, error) {
	workspacePath := filepath.Join(w.TmpRoot, "session-"+sessionID)

	// [C1] Clean up the session directory if setup fails partway through, so
	// partially-created directories don't accumulate in tmp/.
	var setupDone bool
	defer func() {
		if !setupDone {
			os.RemoveAll(workspacePath) //nolint:errcheck
		}
	}()

	if err := os.MkdirAll(w.TmpRoot, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create tmp directory: %w", err)
	}

	agentDir := filepath.Join(workspacePath, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	stateDir := filepath.Join(workspacePath, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create state directory: %w", err)
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
	var missingRepos []string
	for _, repo := range sharedRepos {
		if repo == "" {
			continue
		}
		cachedRepo := filepath.Join(repoCacheRoot, repo)
		info, err := os.Stat(cachedRepo)
		if err != nil || !info.IsDir() {
			// [M7] Track missing repos so the caller can surface them as warnings.
			missingRepos = append(missingRepos, repo)
			if suggestion := SuggestCachedRepo(repoCacheRoot, repo); suggestion != "" {
				slog.Warn("shared repo not found in cache; did you mean a different name?",
					"repo", repo, "suggestion", suggestion)
			} else {
				slog.Warn("shared repo not found in cache", "repo", repo, "cache", repoCacheRoot)
			}
			continue
		}

		dst := filepath.Join(agentDir, repo)
		if err := copyDir(cachedRepo, dst); err != nil {
			slog.Warn("workspace: failed to copy shared repo", "repo", repo, "error", err)
			continue
		}
		slog.Info("workspace: pre-populated shared repo from cache", "repo", repo)

		// Ensure git remote origin is set to the clean URL (no embedded token).
		// Credentials are provided at runtime via a credential helper that reads
		// GIT_TOKEN from the environment — consistent with how memory and job
		// repos handle auth.
		if gitHost != "" && gitOrg != "" {
			cleanURL := fmt.Sprintf("https://%s/%s/%s.git", gitHost, gitOrg, repo)
			configureGitRemote(dst, repo, cleanURL)
			if gitToken != "" {
				configureCredHelper(dst)
			}
		}

		// Fetch latest from origin and reset to remote HEAD so the agent
		// starts with up-to-date code, not just whatever was in the cache.
		// Non-fatal: if fetch fails (no network, bad token) the agent still
		// gets the cached state.
		if err := fetchAndResetRepo(dst, repo); err != nil {
			slog.Warn("workspace: fetch failed, using cached state", "repo", repo, "error", err)
		}
	}

	// Copy env file(s) into workspace. .env.local (if present) is appended
	// so its values override .env when the agent runs "source .env".
	if envFile != "" {
		var combined []byte
		if data, err := os.ReadFile(envFile); err == nil {
			combined = data
		}
		localFile := envFile + ".local"
		if localData, err := os.ReadFile(localFile); err == nil {
			if len(combined) > 0 && combined[len(combined)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, localData...)
			slog.Info("workspace: merged env files", "base", envFile, "local", localFile)
		} else if len(combined) > 0 {
			slog.Info("workspace: copied env file", "src", envFile)
		}
		if len(combined) > 0 {
			_ = os.WriteFile(filepath.Join(agentDir, ".env"), combined, 0600)
		}
	}

	setupDone = true
	return workspacePath, missingRepos, nil
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
			slog.Warn("workspace: failed to cache repo back", "repo", entry.Name(), "error", err)
			continue
		}
		slog.Info("workspace: cached repo back", "repo", entry.Name())
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
	os.RemoveAll(tmp) //nolint:errcheck
	os.RemoveAll(old) //nolint:errcheck

	// Step 1: copy into temp location — original dst is safe if this fails.
	if err := copyDir(src, tmp); err != nil {
		os.RemoveAll(tmp) //nolint:errcheck
		return fmt.Errorf("copy to temp: %w", err)
	}

	// Step 2: move old cache out of the way (no-op if dst doesn't exist yet).
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			os.RemoveAll(tmp) //nolint:errcheck
			return fmt.Errorf("rename old cache aside: %w", err)
		}
	}

	// Step 3: move new copy into place.
	if err := os.Rename(tmp, dst); err != nil {
		// [C2] Attempt to restore the old cache. If the restore also fails,
		// surface both errors so the operator knows the cache entry is gone
		// and which path needs manual repair.
		if _, statErr := os.Stat(old); statErr == nil {
			if restoreErr := os.Rename(old, dst); restoreErr != nil {
				return fmt.Errorf("rename new cache into place: %w; restore of old cache also failed: %v (manual fix: rename %s to %s)", err, restoreErr, old, dst)
			}
		}
		os.RemoveAll(tmp) //nolint:errcheck
		return fmt.Errorf("rename new cache into place: %w", err)
	}

	// Step 4: remove old cache (best-effort; a lingering .old is harmless).
	os.RemoveAll(old) //nolint:errcheck
	return nil
}

// SuggestCachedRepo looks for a cached repo whose normalised name matches
// repo's normalised name. Normalisation folds to lowercase and treats '-' and
// '_' as equivalent, catching the most common typos.
// Returns the suggested name, or "" if nothing close is found.
func SuggestCachedRepo(repoCacheRoot, repo string) string {
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

	// [M10] Reset to the remote HEAD. git clean -fdx is intentionally omitted:
	// the workspace is already a fresh copy from cache so cleaning is unnecessary,
	// and running it would destroy build artifacts that the agent may rely on.
	if err := run("reset", "--hard", "origin/"+branch); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}

	slog.Info("workspace: fetched and reset repo", "repo", repoName, "branch", branch)
	return nil
}

// configureCredHelper sets a git credential helper on the repo that reads
// GIT_TOKEN from the subprocess environment at auth time. The token is never
// written to disk — only the shell expression that reads it is stored.
func configureCredHelper(repoPath string) {
	cmd := exec.Command("git", "config", "credential.helper",
		`!f() { echo username=oauth2; echo "password=$GIT_TOKEN"; }; f`)
	cmd.Dir = repoPath
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("workspace: failed to configure credential helper",
			"path", repoPath, "error", strings.TrimSpace(stderr.String()))
	}
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
			slog.Debug("workspace: git remote already correct", "repo", repoName, "url", expectedURL)
			return
		}
		slog.Info("workspace: updating git remote", "repo", repoName, "old", currentURL, "new", expectedURL)
		setURL := exec.Command("git", "remote", "set-url", "origin", expectedURL)
		setURL.Dir = repoPath
		if err := setURL.Run(); err != nil {
			slog.Warn("workspace: failed to update git remote", "repo", repoName, "error", err)
		}
		return
	}

	// No remote — add it
	slog.Info("workspace: adding git remote", "repo", repoName, "url", expectedURL)
	addRemote := exec.Command("git", "remote", "add", "origin", expectedURL)
	addRemote.Dir = repoPath
	if err := addRemote.Run(); err != nil {
		slog.Warn("workspace: failed to add git remote", "repo", repoName, "error", err)
	}
}

// copyDir recursively copies a directory. Symlinks are resolved to their
// content rather than reproduced, preventing agents from placing links that
// point outside the workspace into persistent storage (memory dir, outputs).
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
		// [H4] Skip symlinks at the directory level — copyFile handles them at
		// the file level, but a symlink to a directory would recurse outside
		// the workspace. Skipping is safer than attempting to resolve.
		if entry.Type()&os.ModeSymlink != 0 {
			slog.Warn("workspace: skipping symlink", "path", filepath.Join(src, entry.Name()))
			continue
		}

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

// copyFile copies a single file. Symlinks are resolved to their content
// rather than reproduced. [H4]
func copyFile(src, dst string) error {
	// Use Lstat so we can detect symlinks before os.ReadFile follows them.
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}

	// [H4] Resolve symlinks to content instead of reproducing the link.
	// Re-creating a symlink whose target points outside the workspace would
	// allow content from arbitrary host paths to land in the memory dir or outputs.
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		data, err := os.ReadFile(src) // follows the symlink to read content
		if err != nil {
			slog.Warn("workspace: skipping unreadable symlink", "src", src, "error", err)
			return nil
		}
		return os.WriteFile(dst, data, 0644)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}
