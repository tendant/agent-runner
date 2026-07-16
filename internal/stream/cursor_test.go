package stream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCursor_NoStateDir(t *testing.T) {
	b := &Bot{}
	if _, ok := b.loadCursor("c_test"); ok {
		t.Error("expected ok=false when stateDir is unset")
	}
}

func TestLoadCursor_NoSavedCursor(t *testing.T) {
	b := &Bot{stateDir: t.TempDir()}
	if _, ok := b.loadCursor("c_test"); ok {
		t.Error("expected ok=false when no cursor file exists yet")
	}
}

func TestSaveCursor_ThenLoadCursor_RoundTrips(t *testing.T) {
	b := &Bot{stateDir: t.TempDir()}
	b.saveCursor("c_test", 42)

	seq, ok := b.loadCursor("c_test")
	if !ok {
		t.Fatal("expected ok=true after saveCursor")
	}
	if seq != 42 {
		t.Errorf("seq = %d, want 42", seq)
	}
}

func TestSaveCursor_Overwrites(t *testing.T) {
	b := &Bot{stateDir: t.TempDir()}
	b.saveCursor("c_test", 5)
	b.saveCursor("c_test", 99)

	seq, ok := b.loadCursor("c_test")
	if !ok || seq != 99 {
		t.Errorf("expected latest saved value 99, got seq=%d ok=%v", seq, ok)
	}
}

func TestSaveCursor_ZeroIsPersistedAndDistinctFromMissing(t *testing.T) {
	// A conversation with genuinely zero events must still short-circuit the
	// catch-up scan on the next restart — saving "0" must count as "ok".
	b := &Bot{stateDir: t.TempDir()}
	b.saveCursor("c_empty", 0)

	seq, ok := b.loadCursor("c_empty")
	if !ok {
		t.Fatal("expected ok=true for a persisted zero cursor")
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0", seq)
	}
}

func TestLoadCursor_CorruptFileFallsBackToNotFound(t *testing.T) {
	dir := t.TempDir()
	b := &Bot{stateDir: dir}
	if err := os.WriteFile(b.cursorPath("c_test"), []byte("not-a-number"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.loadCursor("c_test"); ok {
		t.Error("expected ok=false for a corrupt cursor file")
	}
}

func TestCursorPath_SanitizesConversationID(t *testing.T) {
	b := &Bot{stateDir: "/tmp/state"}
	path := b.cursorPath("c_../../etc/passwd")
	if filepath.Dir(path) != "/tmp/state" {
		t.Errorf("expected cursor path to stay inside stateDir, got %q", path)
	}
	if got := filepath.Base(path); got != "stream-cursor-c_______etc_passwd.txt" {
		t.Errorf("unexpected sanitized filename: %q", got)
	}
}

func TestSaveCursor_DifferentConversationsDoNotCollide(t *testing.T) {
	b := &Bot{stateDir: t.TempDir()}
	b.saveCursor("conv-a", 10)
	b.saveCursor("conv-b", 20)

	seqA, okA := b.loadCursor("conv-a")
	seqB, okB := b.loadCursor("conv-b")
	if !okA || seqA != 10 {
		t.Errorf("conv-a: seq=%d ok=%v, want 10/true", seqA, okA)
	}
	if !okB || seqB != 20 {
		t.Errorf("conv-b: seq=%d ok=%v, want 20/true", seqB, okB)
	}
}
