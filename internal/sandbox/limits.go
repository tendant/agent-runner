package sandbox

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// ErrOutputLimit is returned by a LimitedWriter once the cap is hit.
var ErrOutputLimit = errors.New("sandbox output limit exceeded")

// LimitedWriter caps captured output. Writes past the cap are dropped and
// return ErrOutputLimit, which makes the child's pipe copy fail so the caller
// can terminate the run (resource.output_bytes).
type LimitedWriter struct {
	W   io.Writer
	Max int64
	mu  sync.Mutex
	n   int64
	Hit bool
}

func (l *LimitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Max <= 0 {
		return l.W.Write(p)
	}
	room := l.Max - l.n
	if room <= 0 {
		l.Hit = true
		return 0, ErrOutputLimit
	}
	if int64(len(p)) > room {
		n, _ := l.W.Write(p[:room])
		l.n += int64(n)
		l.Hit = true
		return n, ErrOutputLimit
	}
	n, err := l.W.Write(p)
	l.n += int64(n)
	return n, err
}

// DirSize sums regular-file sizes under root (symlinks are not followed).
func DirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// WatchDisk polls the writable dirs and calls onExceed once when their combined
// size passes max. It is the runner-side disk quota (isobox has no res.disk).
// It returns when ctx ends or the limit is hit.
func WatchDisk(ctx context.Context, dirs []string, max int64, every time.Duration, onExceed func(used int64)) {
	if max <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var used int64
			for _, d := range dirs {
				n, _ := DirSize(d)
				used += n
			}
			if used > max {
				onExceed(used)
				return
			}
		}
	}
}
