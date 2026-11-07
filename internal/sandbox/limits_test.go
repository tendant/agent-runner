package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLimitedWriter(t *testing.T) {
	var b bytes.Buffer
	l := &LimitedWriter{W: &b, Max: 5}
	if _, err := l.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Write([]byte("defgh")); err != ErrOutputLimit || n != 2 {
		t.Errorf("%d %v", n, err)
	}
	if _, err := l.Write([]byte("x")); err != ErrOutputLimit || b.String() != "abcde" || !l.Hit {
		t.Errorf("%q hit=%v", b.String(), l.Hit)
	}
}

func TestWatchDisk(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "f"), bytes.Repeat([]byte("x"), 100), 0o644)
	os.Symlink("/etc/hostname", filepath.Join(d, "link"))
	if n, _ := DirSize(d); n != 100 {
		t.Errorf("size %d", n)
	}
	got := make(chan int64, 1)
	go WatchDisk(context.Background(), []string{d}, 50, 10*time.Millisecond, func(u int64) { got <- u })
	select {
	case u := <-got:
		if u < 100 {
			t.Errorf("used %d", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota never tripped")
	}
}

func TestWatchDiskUnderLimitStopsWithContext(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		WatchDisk(ctx, []string{d}, 1<<30, 5*time.Millisecond, func(int64) { t.Error("tripped under the limit") })
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not end with its context")
	}
}

func TestDiskUsage(t *testing.T) {
	d, e := t.TempDir(), t.TempDir()
	big := bytes.Repeat([]byte("x"), 1<<20)
	os.WriteFile(filepath.Join(d, "f"), big, 0o644)
	base := DiskUsage([]string{d})
	if base < 1<<20 {
		t.Fatalf("1 MiB file counts %d", base)
	}
	// A hard link to it is the same disk, counted once.
	if err := os.Link(filepath.Join(d, "f"), filepath.Join(d, "g")); err == nil {
		if n := DiskUsage([]string{d}); n-base > 64<<10 {
			t.Errorf("hard link counted twice: %d then %d", base, n)
		}
	}
	// Across two dirs, too; and a dir that does not exist counts nothing.
	os.Link(filepath.Join(d, "f"), filepath.Join(e, "h"))
	if n := DiskUsage([]string{d, e, filepath.Join(d, "missing")}); n-base > 128<<10 {
		t.Errorf("link in a second dir counted again: %d then %d", base, n)
	}
	// A sparse file counts what it holds, not its length (unix).
	if runtime.GOOS != "windows" {
		sp := filepath.Join(e, "sparse")
		f, _ := os.Create(sp)
		f.Truncate(1 << 30)
		f.Close()
		if n := DiskUsage([]string{e}); n > 1<<20+256<<10 {
			t.Errorf("1 GiB sparse file counts %d", n)
		}
	}
}

func TestHostMatcher(t *testing.T) {
	m, err := NewHostMatcher([]string{"*.github.com", "10.0.0.0/8", "@vcs"}, map[string][]string{"@vcs": {"gitlab.com"}})
	if err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]bool{"api.github.com": true, "a.b.github.com": false, "github.com": false, "10.2.3.4": true,
		"11.0.0.1": false, "gitlab.com": true, "evil.com": false, "API.GITHUB.COM.": true} {
		if m.Match(h) != want {
			t.Errorf("%s: want %v", h, want)
		}
	}
}
