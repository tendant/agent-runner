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
