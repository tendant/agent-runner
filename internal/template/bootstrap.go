package template

import (
	"os"
	"path/filepath"
)

// IsFirstRun checks if BOOTSTRAP.md exists in the user's templates directory.
// Returns false if templatesDir is empty or BOOTSTRAP.md doesn't exist.
func IsFirstRun(templatesDir string) bool {
	if templatesDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(templatesDir, "BOOTSTRAP.md"))
	return err == nil
}

// CompleteBootstrap renames BOOTSTRAP.md to BOOTSTRAP.md.done after a
// successful first-run session. No-op if the file doesn't exist.
func CompleteBootstrap(templatesDir string) error {
	if templatesDir == "" {
		return nil
	}
	src := filepath.Join(templatesDir, "BOOTSTRAP.md")
	dst := filepath.Join(templatesDir, "BOOTSTRAP.md.done")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return os.Rename(src, dst)
}
