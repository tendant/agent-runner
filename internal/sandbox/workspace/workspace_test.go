package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func fixture(t *testing.T) (thread, run string, base Manifest) {
	d := t.TempDir()
	thread, run = filepath.Join(d, "thread"), filepath.Join(d, "run")
	os.MkdirAll(thread, 0o755)
	write(t, thread, "a.txt", "A")
	write(t, thread, "src/b.go", "B")
	write(t, thread, "old.txt", "O")
	var err error
	if base, err = Stage(thread, run); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRevisionIsContentAddressed(t *testing.T) {
	thread, run, base := fixture(t)
	r2, _ := Snapshot(run)
	if base.Revision != r2.Revision {
		t.Error("identical content must have identical revision")
	}
	write(t, run, "a.txt", "changed")
	r3, _ := Snapshot(run)
	if r3.Revision == base.Revision {
		t.Error("revision must change with content")
	}
	_ = thread
}

func TestExportAppliesWhenThreadUnchanged(t *testing.T) {
	thread, run, base := fixture(t)
	write(t, run, "a.txt", "A2")
	write(t, run, "new/dir/c.txt", "C")
	os.Remove(filepath.Join(run, "old.txt"))
	res, err := Export(thread, run, base, nil, t.TempDir(), "run1")
	if err != nil || res.Outcome != Applied {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, thread, "a.txt") != "A2" || read(t, thread, "new/dir/c.txt") != "C" || read(t, thread, "old.txt") != "<missing>" || read(t, thread, "src/b.go") != "B" {
		t.Error("thread workspace not updated correctly")
	}
}

func TestExportUnchanged(t *testing.T) {
	thread, run, base := fixture(t)
	res, err := Export(thread, run, base, nil, t.TempDir(), "r")
	if err != nil || res.Outcome != Unchanged {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestConflictQuarantinesWithoutLosingWork(t *testing.T) {
	thread, run, base := fixture(t)
	write(t, run, "a.txt", "RUN-EDIT")
	write(t, run, "extra.txt", "RUN-NEW")
	write(t, thread, "a.txt", "THREAD-EDIT") // thread moved after the run started
	q := t.TempDir()
	res, err := Export(thread, run, base, nil, q, "run-conflict")
	if err != nil || res.Outcome != Quarantined || res.Reason != "conflict" {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, thread, "a.txt") != "THREAD-EDIT" || read(t, thread, "extra.txt") != "<missing>" {
		t.Error("thread workspace must be untouched on conflict")
	}
	if read(t, res.QuarantineDir, "files/a.txt") != "RUN-EDIT" || read(t, res.QuarantineDir, "files/extra.txt") != "RUN-NEW" {
		t.Error("run's work must be preserved in quarantine")
	}
	meta := read(t, res.QuarantineDir, "meta.json")
	if !strings.Contains(meta, `"conflict"`) || !strings.Contains(meta, base.Revision) {
		t.Errorf("meta: %s", meta)
	}
}

func TestValidationFailureQuarantines(t *testing.T) {
	thread, run, base := fixture(t)
	write(t, run, ".git/hooks/pre-commit", "evil")
	val := func(ch []string) error {
		for _, c := range ch {
			if strings.HasPrefix(c, ".git/") {
				return errors.New("GIT_DIR_VIOLATION")
			}
		}
		return nil
	}
	res, err := Export(thread, run, base, val, t.TempDir(), "run-bad")
	if err != nil || res.Outcome != Quarantined || !strings.Contains(res.Reason, "GIT_DIR_VIOLATION") {
		t.Fatalf("%+v %v", res, err)
	}
	if read(t, thread, ".git/hooks/pre-commit") != "<missing>" {
		t.Error("invalid diff must not be applied")
	}
}

func TestSymlinkEscapesRejected(t *testing.T) {
	for name, target := range map[string]string{"abs": "/etc/passwd", "rel": "../../../etc/passwd"} {
		thread, run, base := fixture(t)
		os.Symlink(target, filepath.Join(run, "link"))
		res, err := Export(thread, run, base, nil, t.TempDir(), "r-"+name)
		if err != nil || res.Outcome != Quarantined || !strings.Contains(res.Reason, "symlink") {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if _, err := os.Lstat(filepath.Join(thread, "link")); err == nil {
			t.Errorf("%s: link applied", name)
		}
	}
	// An in-workspace relative link is fine.
	thread, run, base := fixture(t)
	os.Symlink("a.txt", filepath.Join(run, "alias"))
	if res, err := Export(thread, run, base, nil, t.TempDir(), "ok"); err != nil || res.Outcome != Applied {
		t.Errorf("%+v %v", res, err)
	}
}

func TestSymlinkedParentDirCannotRedirectWrite(t *testing.T) {
	thread, run, base := fixture(t)
	outside := t.TempDir()
	// The thread workspace gains a symlinked dir after staging is impossible
	// (revision would differ), so model the attack inside the run: replace a
	// real dir with a symlink to an outside path and write "through" it.
	os.RemoveAll(filepath.Join(run, "src"))
	os.Symlink(outside, filepath.Join(run, "src"))
	res, err := Export(thread, run, base, nil, t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Quarantined {
		t.Errorf("absolute symlink replacing a dir must be rejected: %+v", res)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Error("write escaped through symlinked directory")
	}
}

func TestSpecialFilesRejected(t *testing.T) {
	thread, run, base := fixture(t)
	if err := mkfifo(filepath.Join(run, "pipe")); err != nil {
		t.Skip("mkfifo unavailable")
	}
	if _, err := Export(thread, run, base, nil, t.TempDir(), "r"); !errors.Is(err, ErrUnsafe) {
		t.Errorf("fifo: %v", err)
	}
}

func TestBadRunID(t *testing.T) {
	thread, run, base := fixture(t)
	for _, id := range []string{"", "../x", "a/b", ".."} {
		if _, err := Export(thread, run, base, nil, t.TempDir(), id); err == nil {
			t.Errorf("run id %q accepted", id)
		}
	}
}
