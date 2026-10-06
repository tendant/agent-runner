package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
		if u != 100 {
			t.Errorf("used %d", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota never tripped")
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
