package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectScheduleEntries_MissingFile(t *testing.T) {
	dir := t.TempDir()
	entries, err := collectScheduleEntries(dir)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %d", len(entries))
	}
}

func TestCollectScheduleEntries_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	data := `[
		{"message": "daily check", "cron": "0 9 * * *", "timezone": "US/Eastern"},
		{"message": "one-shot", "run_in_seconds": 3600}
	]`
	os.WriteFile(filepath.Join(dir, scheduleFileName), []byte(data), 0644)

	entries, err := collectScheduleEntries(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Message != "daily check" {
		t.Errorf("expected 'daily check', got %q", entries[0].Message)
	}
	if entries[0].Cron != "0 9 * * *" {
		t.Errorf("expected cron '0 9 * * *', got %q", entries[0].Cron)
	}
	if entries[0].Timezone != "US/Eastern" {
		t.Errorf("expected timezone 'US/Eastern', got %q", entries[0].Timezone)
	}
	if entries[1].RunInSeconds != 3600 {
		t.Errorf("expected run_in_seconds 3600, got %d", entries[1].RunInSeconds)
	}
}

func TestCollectScheduleEntries_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, scheduleFileName), []byte("[]"), 0644)

	entries, err := collectScheduleEntries(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestCollectScheduleEntries_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, scheduleFileName), []byte("{not json}"), 0644)

	_, err := collectScheduleEntries(dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestCollectScheduleEntries_AllFields(t *testing.T) {
	dir := t.TempDir()
	data := `[{"message": "test", "run_after": "2025-01-01T00:00:00Z", "idempotency_key": "key-1"}]`
	os.WriteFile(filepath.Join(dir, scheduleFileName), []byte(data), 0644)

	entries, err := collectScheduleEntries(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].RunAfter != "2025-01-01T00:00:00Z" {
		t.Errorf("expected run_after, got %q", entries[0].RunAfter)
	}
	if entries[0].IdempotencyKey != "key-1" {
		t.Errorf("expected idempotency_key 'key-1', got %q", entries[0].IdempotencyKey)
	}
}
