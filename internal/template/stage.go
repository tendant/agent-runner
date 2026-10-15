package template

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
)

// A sandboxed agent cannot write the memory dir: it works on a copy (a
// MemoryStage), and when the session ends the runner merges back only what a
// session may change. That keeps a compromised agent from rewriting its own
// instructions (agent.md, prompt.md) for every later session, and keeps
// concurrent sessions from overwriting each other's notes.

const (
	maxStagedFileBytes = 64 << 10 // how much a session may add to a memory file
	maxStagedNewFiles  = 10       // new memory files per session
)

// stageWritable is the shape of a memory file a session may write: a
// top-level lowercase .md file. The instruction files are upper case
// (HEARTBEAT.md, WELCOME.md, SOUL.md, ...) or listed in stageProtected;
// daily logs are date-prefixed and written by the runner.
var stageWritable = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*\.md$`)

var stageProtected = map[string]bool{"agent.md": true, "prompt.md": true}

// stageAppendOnly files may only grow: the curator owns their rewrites.
var stageAppendOnly = map[string]bool{"lessons.md": true}

// MemoryStage is a session's writable copy of the memory dir.
type MemoryStage struct {
	Dir    string            // the copy the agent is pointed at
	memory string            // the real memory dir
	base   map[string][]byte // what was copied, by file name
}

// StageMemory copies memoryDir's top-level files into dir (replacing what
// was there) for a sandboxed session to work on. Subdirectories, dotfiles
// and links are not copied; they stay readable in the memory dir itself.
func StageMemory(memoryDir, dir string) (*MemoryStage, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &MemoryStage{Dir: dir, memory: memoryDir, base: map[string][]byte{}}
	err := WithMemoryLock(func() error {
		ents, err := os.ReadDir(memoryDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !e.Type().IsRegular() || e.Name()[0] == '.' {
				continue
			}
			data, err := os.ReadFile(filepath.Join(memoryDir, e.Name()))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
				return err
			}
			st.base[e.Name()] = data
		}
		return nil
	})
	if err != nil {
		os.RemoveAll(dir) //nolint:errcheck
		return nil, fmt.Errorf("stage memory: %w", err)
	}
	return st, nil
}

// MergeResult is what Merge saved and what it refused, with why.
type MergeResult struct {
	Saved   []string
	Skipped []string // "name: reason"
}

// Merge applies the session's changes to the memory dir and removes the
// copy. Only changed, allowed files are written. A file another writer
// changed meanwhile takes the session's change only if it is a pure append,
// added to the current content. Deletions are not applied.
func (st *MemoryStage) Merge() (MergeResult, error) {
	var res MergeResult
	if st == nil {
		return res, nil
	}
	defer os.RemoveAll(st.Dir) //nolint:errcheck
	ents, err := os.ReadDir(st.Dir)
	if err != nil {
		return res, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	skip := func(name, why string) { res.Skipped = append(res.Skipped, name+": "+why) }
	newFiles := 0
	for _, name := range names {
		base, existed := st.base[name]
		data, err := readStaged(filepath.Join(st.Dir, name), len(base)+maxStagedFileBytes)
		switch {
		case err == errNotRegular:
			skip(name, "not a regular file")
			continue
		case err == errTooLarge:
			skip(name, fmt.Sprintf("grew by more than %d KiB", maxStagedFileBytes>>10))
			continue
		case err != nil:
			return res, err
		case existed && bytes.Equal(data, base):
			continue // unchanged
		case !stageWritable.MatchString(name) || stageProtected[name] || datePrefix.MatchString(name):
			skip(name, "not writable from the sandbox")
			continue
		case bytes.IndexByte(data, 0) >= 0:
			skip(name, "binary content")
			continue
		case stageAppendOnly[name] && !bytes.HasPrefix(data, base):
			skip(name, "append-only")
			continue
		case !existed:
			if newFiles++; newFiles > maxStagedNewFiles {
				skip(name, fmt.Sprintf("more than %d new files", maxStagedNewFiles))
				continue
			}
		}
		saved, why, err := st.apply(name, base, existed, data)
		if err != nil {
			return res, err
		}
		if saved {
			res.Saved = append(res.Saved, name)
		} else {
			skip(name, why)
		}
	}
	return res, nil
}

// apply writes one changed file under the memory lock, three-way: the
// session's version if the file is as it was copied, else its appended tail.
func (st *MemoryStage) apply(name string, base []byte, existed bool, data []byte) (bool, string, error) {
	saved, why := false, ""
	err := WithMemoryLock(func() error {
		cur, err := os.ReadFile(filepath.Join(st.memory, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		curExists := err == nil
		switch {
		case curExists == existed && bytes.Equal(cur, base):
			// as copied
		case existed && curExists && bytes.HasPrefix(data, base):
			data = append(append([]byte{}, cur...), data[len(base):]...)
		default:
			why = "changed by another session meanwhile"
			return nil
		}
		saved = true
		return writeMemoryFile(st.memory, name, string(data))
	})
	return saved, why, err
}

var (
	errNotRegular = fmt.Errorf("not a regular file")
	errTooLarge   = fmt.Errorf("too large")
)

// readStaged reads a file the sandbox wrote without following a link it may
// have planted in its place (to a host file the runner can read).
func readStaged(path string, limit int) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if fi, lerr := os.Lstat(path); lerr == nil && !fi.Mode().IsRegular() {
			return nil, errNotRegular
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errTooLarge
	}
	return data, nil
}
