package template

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReadMemoryFile reads memoryDir/name and returns its contents.
func ReadMemoryFile(memoryDir, name string) (string, error) {
	return readMemoryFile(memoryDir, name)
}

// readMemoryFile is the unlocked core of ReadMemoryFile, for callers that
// already hold memoryMu.
func readMemoryFile(memoryDir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(memoryDir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteMemoryFile writes content to memoryDir/name, creating the directory if needed.
func WriteMemoryFile(memoryDir, name, content string) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return writeMemoryFile(memoryDir, name, content)
}

// writeMemoryFile is the unlocked core of WriteMemoryFile, for callers that
// already hold memoryMu.
//
// The write goes to a temp file and is renamed into place. A plain os.WriteFile
// truncates first, so a concurrent reader — the prompt compiler, the curator, or
// `git add -A` during a push — can observe the file empty or half-written and
// commit that. Rename is atomic, so a reader sees either the old file or the new
// one. Same approach as internal/sessionjournal.
func writeMemoryFile(memoryDir, name, content string) error {
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	f, err := os.CreateTemp(memoryDir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := f.Name()

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(memoryDir, name)); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// AppendDailyLog appends a timestamped entry to today's daily log file
// (memoryDir/YYYY-MM-DD-<hostname>.md). Creates the directory if needed.
//
// The filename carries no session ID, so concurrent sessions on the same host
// share one file. The lock keeps their entries whole and in timestamp order.
func AppendDailyLog(memoryDir, entry string) error {
	if memoryDir == "" {
		return nil
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	filename := time.Now().Format("2006-01-02") + "-" + sanitizeForFilename(hostname) + ".md"
	path := filepath.Join(memoryDir, filename)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open daily log: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry + "\n"); err != nil {
		return fmt.Errorf("write daily log: %w", err)
	}
	return nil
}

// sanitizeForFilename replaces characters unsafe in filenames with hyphens.
func sanitizeForFilename(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	result := b.String()
	if result == "" {
		return "local"
	}
	return result
}
