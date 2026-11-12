package template

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The memory lock holds across processes: while another holder (here an
// flock on its own open file, as another process's would be) has the lock
// file, a write waits for it.
func TestMemoryLockAcrossProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	wrote := make(chan error, 1)
	go func() {
		wrote <- UpdateMemoryFile(dir, "notes.md", func(old string) (string, error) { return old + "x\n", nil })
	}()
	select {
	case <-wrote:
		t.Fatal("wrote while another process held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never got the lock")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.md")); string(b) != "x\n" {
		t.Errorf("notes.md = %q", b)
	}
}
