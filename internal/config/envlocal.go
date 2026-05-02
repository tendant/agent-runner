package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// envLocalPath is the absolute path to .env.local. Set by LoadFromEnv to
// DATA_DIR/.env.local (default: ~/.agent-runner/.env.local).
var envLocalPath = ".env.local"

// SetEnvLocal writes or updates a single key in the data-dir .env.local.
// Other keys already present in the file are preserved.
// The file and its parent directory are created if they do not exist.
func SetEnvLocal(key, value string) error {
	if err := os.MkdirAll(filepath.Dir(envLocalPath), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return setEnvLocalFile(envLocalPath, key, value)
}

func setEnvLocalFile(path, key, value string) error {
	// Read existing content (ignore if file doesn't exist yet).
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	}

	lines := strings.Split(existing, "\n")
	updated := false
	prefix := key + "="

	for i, line := range lines {
		if strings.HasPrefix(line, prefix) || line == key {
			lines[i] = fmt.Sprintf("%s=%s", key, value)
			updated = true
			break
		}
	}
	if !updated {
		// Strip trailing blank line before appending so we don't accumulate blanks.
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, fmt.Sprintf("%s=%s", key, value))
	}

	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0600)
}
