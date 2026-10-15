// Package safefs is how agent-runner, on the host, reads and writes files
// under a directory a sandboxed agent can write to (its workspace, its
// sandbox home). The agent may have replaced any of it with links, FIFOs or
// huge files, so nothing here follows a link out of the directory, reads
// anything but a regular file, or reads without a size limit; writes go to
// a temporary file renamed into place, inside the directory.
package safefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

var (
	// ErrNotRegular is a name that is not a regular file (a link out of the
	// directory, a FIFO, a device, a directory).
	ErrNotRegular = errors.New("not a regular file")
	// ErrTooLarge is a file over the read limit.
	ErrTooLarge = errors.New("file too large")
)

// Open opens dir as a root that nothing under it can escape: dir itself
// must be a real directory, not a link.
func Open(dir string) (*os.Root, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return os.OpenRoot(dir)
}

// Read reads name (relative, under dir) up to limit bytes.
func Read(dir, name string, limit int64) ([]byte, error) {
	root, err := Open(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return ReadIn(root, name, limit)
}

// ReadIn is Read under an open root.
func ReadIn(root *os.Root, name string, limit int64) ([]byte, error) {
	if fi, err := root.Lstat(name); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Write writes data to name (relative, under dir) through a temporary file
// renamed into place: a link planted at name is replaced, not written
// through. Missing parent directories are created inside dir.
func Write(dir, name string, data []byte, perm fs.FileMode) error {
	root, err := Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return WriteIn(root, name, data, perm)
}

// WriteIn is Write under an open root.
func WriteIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	tmp := filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+".tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		root.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp) //nolint:errcheck
		return err
	}
	return nil
}
