package sessionjournal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournal_RoundTrip(t *testing.T) {
	j, err := New(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	entry := Entry{
		SessionID:       "agent-abc",
		Source:          "telegram",
		ConvID:          "12345",
		Message:         "do the thing",
		MaxIterations:   5,
		MaxTotalSeconds: 300,
		Status:          "queued",
	}
	if err := j.Write(entry); err != nil {
		t.Fatal(err)
	}

	entries, err := j.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.SessionID != "agent-abc" || got.ConvID != "12345" || got.Status != "queued" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("expected timestamps to be stamped")
	}
}

func TestJournal_UpdateOverwrites(t *testing.T) {
	j, _ := New(filepath.Join(t.TempDir(), "sessions"))
	j.Write(Entry{SessionID: "s1", Status: "queued"})
	j.Write(Entry{SessionID: "s1", Status: "running"})

	entries, _ := j.LoadAll()
	if len(entries) != 1 || entries[0].Status != "running" {
		t.Errorf("expected single running entry, got %+v", entries)
	}
}

func TestJournal_RemoveIdempotent(t *testing.T) {
	j, _ := New(filepath.Join(t.TempDir(), "sessions"))
	j.Write(Entry{SessionID: "s1", Status: "queued"})

	if err := j.Remove("s1"); err != nil {
		t.Fatal(err)
	}
	if err := j.Remove("s1"); err != nil {
		t.Errorf("removing a missing entry must not error, got %v", err)
	}
	entries, _ := j.LoadAll()
	if len(entries) != 0 {
		t.Errorf("expected empty journal, got %d entries", len(entries))
	}
}

func TestJournal_CorruptEntryDeleted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	j, _ := New(dir)
	j.Write(Entry{SessionID: "good", Status: "queued"})
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0644)
	os.WriteFile(filepath.Join(dir, "good2.tmp-123"), []byte("leftover"), 0644)

	entries, err := j.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != "good" {
		t.Errorf("expected only the good entry, got %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Error("corrupt entry should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "good2.tmp-123")); !os.IsNotExist(err) {
		t.Error("leftover temp file should be cleaned up")
	}
}
