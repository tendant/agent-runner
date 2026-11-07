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

// DiskUsage is the disk the files under the dirs take: allocated blocks
// where the system reports them (a sparse file counts what it holds, not its
// length), each hard-linked file once, symlinks not followed. A dir that does
// not exist counts nothing; one that cannot be read in part counts the rest.
func DiskUsage(dirs []string) int64 {
	var total int64
	seen := map[fileID]bool{}
	for _, root := range dirs {
		_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable or vanished mid-walk: count the rest
			}
			if !d.Type().IsRegular() && !d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			n, id, ok := allocated(info)
			if ok && id.links > 1 {
				if seen[id.key()] {
					return nil
				}
				seen[id.key()] = true
			}
			total += n
			return nil
		})
	}
	return total
}

// WatchDisk polls the dirs' DiskUsage and calls onExceed once when it passes
// limit. It is the runner-side disk quota (no backend enforces res.disk). A
// walk of a large tree is not free, so the polls are spaced at least every
// apart and at least ten times the last walk's duration (up to a minute):
// what is written between polls can overshoot the cap. It returns when ctx
// ends or the limit is hit.
func WatchDisk(ctx context.Context, dirs []string, limit int64, every time.Duration, onExceed func(used int64)) {
	if limit <= 0 {
		return
	}
	wait := every
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		start := time.Now()
		used := DiskUsage(dirs)
		if ctx.Err() != nil {
			return
		}
		if used > limit {
			onExceed(used)
			return
		}
		wait = min(maxDiskPoll, 10*time.Since(start))
		wait = max(wait, every)
	}
}

// maxDiskPoll bounds how far apart WatchDisk's polls grow on a large tree.
const maxDiskPoll = time.Minute
