package executor

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
func (w *WorkspaceManager) PrepareAgentWorkspace(reposRoot, sessionID string, sharedRepos []string, gitHost, gitOrg string) (string, error) {
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
		cachedRepo := filepath.Join(reposRoot, repo)
		if info, err := os.Stat(cachedRepo); err == nil && info.IsDir() {
			dst := filepath.Join(reposPath, repo)
			if err := copyDir(cachedRepo, dst); err != nil {
				log.Printf("Agent workspace: warning: failed to copy shared repo %s: %v", repo, err)
				continue
			}
			log.Printf("Agent workspace: pre-populated shared repo %s from cache", repo)

			// Configure git remote if git host/org are set
			if gitHost != "" && gitOrg != "" {
				remoteURL := fmt.Sprintf("https://%s/%s/%s.git", gitHost, gitOrg, repo)
				setRemote := exec.Command("git", "remote", "set-url", "origin", remoteURL)
				setRemote.Dir = dst
				if err := setRemote.Run(); err != nil {
					// If set-url fails (no remote yet), try adding it
					addRemote := exec.Command("git", "remote", "add", "origin", remoteURL)
					addRemote.Dir = dst
					if addErr := addRemote.Run(); addErr != nil {
						log.Printf("Agent workspace: warning: failed to set git remote for %s: %v", repo, addErr)
					}
				}
				log.Printf("Agent workspace: set git remote for %s to %s", repo, remoteURL)
			}
		}
	}

	return workspacePath, nil
}

// CacheReposBack copies repos from workspace/repos/ back to reposRoot/ for future runs.
func (w *WorkspaceManager) CacheReposBack(workspacePath, reposRoot string) {
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
		dst := filepath.Join(reposRoot, entry.Name())

		// Remove old cache and replace with updated version
		os.RemoveAll(dst)
		if err := copyDir(src, dst); err != nil {
			log.Printf("Agent workspace: warning: failed to cache repo %s: %v", entry.Name(), err)
			continue
		}
		log.Printf("Agent workspace: cached repo %s back to repos", entry.Name())
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
