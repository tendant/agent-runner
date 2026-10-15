package execution

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A sandboxed agent can edit its repos' .git/config, but the runner runs git
// on the host in those repos (push, rebase, cache-back, the next session's
// fetch). gitsafe neutralizes the settings it knows about; this goes further
// and never lets the runner use a config the agent wrote. Each repo's config
// is saved as the runner left it after workspace preparation, outside the
// sandbox's view, and put back before every host-side git step. Repos with
// no saved config (the agent made them) are not pushed or cached.

// trustedGitDir holds the saved configs, next to the agent's checkout: in
// the session workspace, which the sandbox cannot see.
func trustedGitDir(checkoutPath string) string {
	return filepath.Join(filepath.Dir(checkoutPath), ".trusted-git")
}

func trustedConfigPath(checkoutPath, repo string) string {
	rel, err := filepath.Rel(checkoutPath, repo)
	if err != nil || rel == "." {
		rel = "_root"
	}
	return filepath.Join(trustedGitDir(checkoutPath), strings.ReplaceAll(rel, string(filepath.Separator), "__")+".config")
}

// saveGitConfigs records each repo's config as the runner prepared it. A
// config already saved (an earlier turn of a task) is kept: the agent has
// had the workspace since.
func saveGitConfigs(checkoutPath string) error {
	if err := os.MkdirAll(trustedGitDir(checkoutPath), 0o700); err != nil {
		return err
	}
	for _, repo := range workspaceRepos(checkoutPath) {
		dst := trustedConfigPath(checkoutPath, repo)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		cfg, err := regularGitConfig(repo)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(cfg)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// restoreGitConfigs puts the saved configs back and returns the repos it
// trusts. The others are reported in untrusted.
func restoreGitConfigs(checkoutPath string) (trusted, untrusted []string) {
	for _, repo := range workspaceRepos(checkoutPath) {
		data, err := os.ReadFile(trustedConfigPath(checkoutPath, repo))
		if err == nil {
			err = writeGitConfig(repo, data)
		}
		if err != nil {
			untrusted = append(untrusted, repo)
			continue
		}
		trusted = append(trusted, repo)
	}
	return trusted, untrusted
}

// regularGitConfig is repo/.git/config, provided .git is a directory and the
// config a plain file (neither may be a link to another repository).
func regularGitConfig(repo string) (string, error) {
	gitDir := filepath.Join(repo, ".git")
	fi, err := os.Lstat(gitDir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", gitDir)
	}
	cfg := filepath.Join(gitDir, "config")
	if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", cfg)
	}
	return cfg, nil
}

// writeGitConfig replaces repo/.git/config with data, whatever is there.
func writeGitConfig(repo string, data []byte) error {
	gitDir := filepath.Join(repo, ".git")
	fi, err := os.Lstat(gitDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", gitDir)
	}
	tmp, err := os.CreateTemp(gitDir, ".config.trusted-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	tmp.Close()
	// Rename replaces a planted link or directory entry, not its target.
	cfg := filepath.Join(gitDir, "config")
	if fi, err := os.Lstat(cfg); err == nil && fi.IsDir() {
		if err := os.RemoveAll(cfg); err != nil {
			os.Remove(tmp.Name()) //nolint:errcheck
			return err
		}
	}
	if err := os.Rename(tmp.Name(), cfg); err != nil {
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	return nil
}

// cacheReposBack copies the workspace's repos to the cache. Sandboxed, the
// saved configs go back first, and repos without one are left out.
func (h *Engine) cacheReposBack(workspacePath string) {
	var skip map[string]bool
	if h.sandboxed() {
		_, untrusted := restoreGitConfigs(filepath.Join(workspacePath, "workspace"))
		skip = map[string]bool{}
		for _, r := range untrusted {
			skip[filepath.Base(r)] = true
		}
	}
	h.workspaceManager.CacheReposBackExcept(workspacePath, h.config.RepoCacheRoot, skip)
}
