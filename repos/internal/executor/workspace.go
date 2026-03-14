package executor

import (
	"fmt"
	"log"
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

// PrepareAgentWorkspace creates a workspace with a repos/ subdirectory.
// It pre-populates shared repos from the persistent repo cache.
// If gitHost and gitOrg are set, it configures the git remote origin for each repo.
func (w *WorkspaceManager) PrepareAgentWorkspace(workspacesRoot, sessionID string, sharedRepos []string, gitHost, gitOrg string) (string, error) {
	workspacePath := filepath.Join(w.TmpRoot, "session-"+sessionID)

	if err := os.MkdirAll(w.TmpRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create tmp directory: %w", err)
	}

	reposPath := filepath.Join(workspacePath, "repos")
	if err := os.MkdirAll(reposPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create repos directory: %w", err)
	}

	// Pre-populate shared repos from cache
	for _, repo := range sharedRepos {
		if repo == "" {
			continue
		}
		cachedRepo := filepath.Join(workspacesRoot, repo)
		if info, err := os.Stat(cachedRepo); err == nil && info.IsDir() {
			dst := filepath.Join(reposPath, repo)
			if err := copyDir(cachedRepo, dst); err != nil {
				log.Printf("Agent workspace: warning: failed to copy shared repo %s: %v", repo, err)
				continue
			}
			log.Printf("Agent workspace: pre-populated shared repo %s from cache", repo)

			// Ensure git remote origin matches expected URL
			if gitHost != "" && gitOrg != "" {
				expectedURL := fmt.Sprintf("https://%s/%s/%s.git", gitHost, gitOrg, repo)
				configureGitRemote(dst, repo, expectedURL)
			}
		}
	}

	return workspacePath, nil
}

// CacheReposBack copies repos from workspace/repos/ back to workspacesRoot/ for future runs.
func (w *WorkspaceManager) CacheReposBack(workspacePath, workspacesRoot string) {
	reposPath := filepath.Join(workspacePath, "repos")
	entries, err := os.ReadDir(reposPath)
	if err != nil {
		return // no repos directory, nothing to cache
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		src := filepath.Join(reposPath, entry.Name())
		dst := filepath.Join(workspacesRoot, entry.Name())

		// Remove old cache and replace with updated version
		os.RemoveAll(dst)
		if err := copyDir(src, dst); err != nil {
			log.Printf("Agent workspace: warning: failed to cache repo %s: %v", entry.Name(), err)
			continue
		}
		log.Printf("Agent workspace: cached repo %s back to repos", entry.Name())
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
