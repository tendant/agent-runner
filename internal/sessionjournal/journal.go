// Package sessionjournal persists a small on-disk record of every queued or
// running agent session so a restarted server can recover them: sessions that
// never started are re-enqueued, interrupted ones are failed with a
// notification to their originating conversation. Entries are removed when a
// session reaches a terminal state — an empty journal directory means a clean
// shutdown.
package sessionjournal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is the persisted state of one active session — enough to rebuild a
// queued session or describe an interrupted one.
type Entry struct {
	SessionID       string    `json:"session_id"`
	Source          string    `json:"source"`
	ConvID          string    `json:"conv_id,omitempty"`
	Message         string    `json:"message"`
	Paths           []string  `json:"paths,omitempty"`
	Author          string    `json:"author,omitempty"`
	CommitPrefix    string    `json:"commit_prefix,omitempty"`
	MaxIterations   int       `json:"max_iterations"`
	MaxTotalSeconds int       `json:"max_total_seconds"`
	Status          string    `json:"status"` // "queued" | "running"
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Journal stores one JSON file per active session under dir.
type Journal struct {
	dir string
}

// New creates a Journal rooted at dir, creating it if needed.
func New(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	return &Journal{dir: dir}, nil
}

func (j *Journal) path(sessionID string) string {
	return filepath.Join(j.dir, sessionID+".json")
}

// Write persists entry atomically (temp file + rename in the same dir).
func (j *Journal) Write(entry Entry) error {
	entry.UpdatedAt = time.Now()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = entry.UpdatedAt
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal journal entry: %w", err)
	}
	tmp, err := os.CreateTemp(j.dir, entry.SessionID+".tmp-*")
	if err != nil {
		return fmt.Errorf("create journal temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write journal entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close journal temp file: %w", err)
	}
	if err := os.Rename(tmpName, j.path(entry.SessionID)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename journal entry: %w", err)
	}
	return nil
}

// Remove deletes a session's entry. Missing entries are not an error.
func (j *Journal) Remove(sessionID string) error {
	err := os.Remove(j.path(sessionID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadAll returns every persisted entry. Corrupt files are deleted with a
// warning (same policy as conversation persistence); leftover temp files are
// cleaned up silently.
func (j *Journal) LoadAll() ([]Entry, error) {
	dirEntries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, fmt.Errorf("read journal dir: %w", err)
	}
	var entries []Entry
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			if strings.Contains(de.Name(), ".tmp-") {
				os.Remove(filepath.Join(j.dir, de.Name()))
			}
			continue
		}
		path := filepath.Join(j.dir, de.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("session journal: unreadable entry skipped", "path", path, "error", err)
			continue
		}
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil || entry.SessionID == "" {
			slog.Warn("session journal: corrupt entry deleted", "path", path, "error", err)
			os.Remove(path)
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
