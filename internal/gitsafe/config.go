package gitsafe

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteConfig replaces repo/.git/config with data, whatever is there (a
// link or directory planted in its place is replaced, not written through):
// the runner putting back a config it saved before an agent could edit it.
func WriteConfig(repo string, data []byte) error {
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

// redirecting are files in .git that make git read its config, objects or
// refs from elsewhere, around a config the runner restored.
var redirecting = []string{"commondir", "config.worktree", "objects/info/alternates", "objects/info/http-alternates"}

// CheckRepo refuses a repo the runner must not run git in on the host after
// an agent had it: .git not a real directory, its config not a regular file,
// or a file that points git somewhere else.
func CheckRepo(repo string) error {
	gitDir := filepath.Join(repo, ".git")
	fi, err := os.Lstat(gitDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf(".git is not a directory")
	}
	if fi, err := os.Lstat(filepath.Join(gitDir, "config")); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf(".git/config is not a regular file")
	}
	for _, name := range redirecting {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return fmt.Errorf(".git/%s is not accepted (it points git elsewhere)", name)
		}
	}
	return nil
}

// RepoEnv pins git to repo's own .git for a host command, whatever files in
// it say.
func RepoEnv(repo string) []string {
	gitDir := filepath.Join(repo, ".git")
	return []string{"GIT_DIR=" + gitDir, "GIT_COMMON_DIR=" + gitDir, "GIT_WORK_TREE=" + repo}
}
