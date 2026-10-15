package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadRefusesWhatIsNotAFileInside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("host secret"), 0o600)
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))
	syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644)
	os.WriteFile(filepath.Join(dir, "big"), []byte(strings.Repeat("x", 100)), 0o644)

	if b, err := Read(dir, "ok.txt", 10); err != nil || string(b) != "ok" {
		t.Errorf("regular file: %q %v", b, err)
	}
	for _, name := range []string{"link", "fifo"} {
		if b, err := Read(dir, name, 1<<20); err == nil {
			t.Errorf("%s: read %q", name, b)
		}
	}
	if _, err := Read(dir, "big", 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big: %v", err)
	}
	if _, err := Read(dir, "../"+filepath.Base(outside), 1<<20); err == nil {
		t.Error("a path out of the directory must fail")
	}
	// The directory itself replaced by a link.
	linkDir := filepath.Join(t.TempDir(), "d")
	os.Symlink(filepath.Dir(outside), linkDir)
	if _, err := Read(linkDir, "secret", 1<<20); err == nil {
		t.Error("a linked directory must be refused")
	}
}

func TestWriteReplacesALinkInsteadOfWritingThrough(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "runner.json")
	os.WriteFile(outside, []byte("runner"), 0o600)
	os.Symlink(outside, filepath.Join(dir, "_progress.json"))
	if err := Write(dir, "_progress.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "runner" {
		t.Errorf("wrote through the link: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "_progress.json")); !fi.Mode().IsRegular() {
		t.Error("the link should be replaced by a file")
	}
	// A linked subdirectory cannot carry a write out.
	os.Symlink(filepath.Dir(outside), filepath.Join(dir, "sub"))
	if err := Write(dir, "sub/x", []byte("x"), 0o644); err == nil {
		t.Error("a write through a linked subdirectory must fail")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "x")); err == nil {
		t.Error("wrote outside")
	}
}
