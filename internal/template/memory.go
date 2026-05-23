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
	data, err := os.ReadFile(filepath.Join(memoryDir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteMemoryFile writes content to memoryDir/name, creating the directory if needed.
func WriteMemoryFile(memoryDir, name, content string) error {
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	return os.WriteFile(filepath.Join(memoryDir, name), []byte(content), 0644)
}

// AppendDailyLog appends a timestamped entry to today's daily log file
// (memoryDir/YYYY-MM-DD-<hostname>.md). Creates the directory if needed.
func AppendDailyLog(memoryDir, entry string) error {
	if memoryDir == "" {
		return nil
	}
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
