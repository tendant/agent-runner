package template

import (
	"fmt"
	"strings"
	"sync"
)

// memoryMu serialises writes to the shared memory dir.
//
// MEMORY_DIR is a single git-backed directory that every agent session reads at
// start, writes to on completion (daily log, curation), and commits and pushes.
// The chat /memory commands touch the same directory from their own goroutines.
// Before sessions could overlap this was safe by construction; it no longer is.
// Unsynchronised, concurrent callers collide on .git/index.lock, abort each
// other's rebases, and lose read-modify-write updates.
//
// The lock covers this process only. The agent CLI subprocess is also told to
// edit memory files directly (see internal/clisetup), and no in-process lock can
// cover that — the git history remains the undo mechanism there.
var memoryMu sync.Mutex

// WithMemoryLock runs fn holding the memory-dir lock.
//
// The lock is NOT reentrant: fn must not call an exported memory function that
// takes the lock itself (WriteMemoryFile, AppendDailyLog, UpdateMemoryFile,
// PullMemory, CommitAndPushMemory). Use it to make a sequence of raw file or git
// operations atomic with respect to other memory writers.
//
// Do not hold it across an LLM call or any other long, network-bound operation;
// use WriteMemoryFileIfUnchanged for read-think-write cycles instead.
func WithMemoryLock(fn func() error) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return fn()
}

// UpdateMemoryFile atomically applies fn to the current contents of
// memoryDir/name and writes the result back, holding the memory lock across the
// read and the write. A missing file is presented to fn as "".
//
// This is the safe form of the read-concat-write cycle; doing it with
// ReadMemoryFile followed by WriteMemoryFile loses one update when two callers
// interleave.
func UpdateMemoryFile(memoryDir, name string, fn func(existing string) (string, error)) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()

	existing, err := readMemoryFile(memoryDir, name)
	if err != nil {
		existing = ""
	}
	updated, err := fn(existing)
	if err != nil {
		return err
	}
	return writeMemoryFile(memoryDir, name, updated)
}

// WriteMemoryFileIfUnchanged writes content to memoryDir/name only if the file
// still holds expected, comparing with leading and trailing whitespace trimmed.
// It reports whether the write happened.
//
// This is the compare-and-swap for read-think-write cycles whose "think" step is
// too slow to hold the lock through — curation reads a file, spends seconds in
// an LLM call, then writes a rewrite of what it read. Without the check that
// rewrite silently discards anything another writer appended in between.
func WriteMemoryFileIfUnchanged(memoryDir, name, expected, content string) (bool, error) {
	memoryMu.Lock()
	defer memoryMu.Unlock()

	current, err := readMemoryFile(memoryDir, name)
	if err != nil {
		return false, fmt.Errorf("read for compare: %w", err)
	}
	if strings.TrimSpace(current) != strings.TrimSpace(expected) {
		return false, nil
	}
	if err := writeMemoryFile(memoryDir, name, content); err != nil {
		return false, err
	}
	return true, nil
}
