package template

import (
	"os"
	"path/filepath"
)

// IsFirstRun checks if BOOTSTRAP.md exists in the memory directory.
// Returns false if memoryDir is empty or BOOTSTRAP.md doesn't exist.
func IsFirstRun(memoryDir string) bool {
	if memoryDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(memoryDir, "BOOTSTRAP.md"))
	return err == nil
}

// CompleteBootstrap renames BOOTSTRAP.md to BOOTSTRAP.md.done after a
// successful first-run session. No-op if the file doesn't exist.
func CompleteBootstrap(memoryDir string) error {
	if memoryDir == "" {
		return nil
	}
	src := filepath.Join(memoryDir, "BOOTSTRAP.md")
	dst := filepath.Join(memoryDir, "BOOTSTRAP.md.done")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return os.Rename(src, dst)
}
