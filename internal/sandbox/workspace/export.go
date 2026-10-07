package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Validator checks the changed paths (e.g. executor.Validator.ValidateDiff).
// Returning an error rejects the diff.
type Validator func(changed []string) error

// Outcome says what happened to a run's changes.
type Outcome string

const (
	Applied     Outcome = "applied"
	Unchanged   Outcome = "unchanged"
	Quarantined Outcome = "quarantined"
)

// Result of Export.
type Result struct {
	Outcome       Outcome
	Changes       []Change
	Reason        string // why quarantined: "conflict" or "invalid: ..."
	QuarantineDir string
}

// Meta is stored beside quarantined files.
type Meta struct {
	RunID          string    `json:"run_id"`
	At             time.Time `json:"at"`
	BaseRevision   string    `json:"base_revision"`
	ThreadRevision string    `json:"thread_revision"`
	Reason         string    `json:"reason"`
	Changes        []Change  `json:"changes"`
}

// ErrUnsafe wraps structural problems in the sandbox's output.
var ErrUnsafe = errors.New("unsafe workspace output")

// Export applies the run's changes to threadDir if and only if the Thread
// workspace still equals base and the diff validates; otherwise the diff is
// written to quarantineRoot/<runID> and threadDir is left untouched. The caller
// must hold the Thread lease so nothing else mutates threadDir meanwhile.
func Export(threadDir, runDir string, base Manifest, validate Validator, quarantineRoot, runID string) (Result, error) {
	if runID == "" || strings.ContainsAny(runID, "/\\") || runID == "." || runID == ".." {
		return Result{}, fmt.Errorf("bad run id %q", runID)
	}
	after, err := Snapshot(runDir)
	if err != nil {
		// Special files or unreadable output: keep what we can for inspection.
		return Result{}, fmt.Errorf("%w: %v", ErrUnsafe, err)
	}
	changes := Diff(base, after)
	if len(changes) == 0 {
		return Result{Outcome: Unchanged}, nil
	}
	cur, err := Snapshot(threadDir)
	if err != nil {
		return Result{}, err
	}
	reason := ""
	switch {
	case cur.Revision != base.Revision:
		reason = "conflict"
	default:
		if reason = checkChanges(changes, after, validate); reason != "" {
			reason = "invalid: " + reason
		}
	}
	if reason != "" {
		dir, qerr := quarantine(quarantineRoot, runID, runDir, Meta{RunID: runID, At: time.Now().UTC(),
			BaseRevision: base.Revision, ThreadRevision: cur.Revision, Reason: reason, Changes: changes}, after)
		if qerr != nil {
			return Result{}, fmt.Errorf("quarantine failed (run dir kept at %s): %w", runDir, qerr)
		}
		return Result{Outcome: Quarantined, Changes: changes, Reason: reason, QuarantineDir: dir}, nil
	}
	if err := apply(threadDir, runDir, changes, after); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Applied, Changes: changes}, nil
}

func checkChanges(changes []Change, after Manifest, validate Validator) string {
	var paths []string
	for _, c := range changes {
		if err := SafePath(c.Path); err != nil {
			return err.Error()
		}
		paths = append(paths, c.Path)
		if c.Op == Delete {
			continue
		}
		if e := after.Files[c.Path]; e.Link != "" {
			// A symlink must stay inside the workspace.
			t := e.Link
			if filepath.IsAbs(t) {
				return fmt.Sprintf("symlink %s targets absolute path", c.Path)
			}
			res := filepath.Clean(filepath.Join(filepath.Dir(c.Path), t))
			if res == ".." || strings.HasPrefix(res, "../") {
				return fmt.Sprintf("symlink %s escapes workspace", c.Path)
			}
		}
	}
	if validate != nil {
		if err := validate(paths); err != nil {
			return err.Error()
		}
	}
	return ""
}

// apply writes changes into threadDir: deletions deepest-first, then adds and
// modifications shallowest-first. Parent symlinks are refused so a symlinked
// directory can never redirect a write outside the workspace.
func apply(threadDir, runDir string, changes []Change, after Manifest) error {
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if c.Op != Delete {
			continue
		}
		p := filepath.Join(threadDir, filepath.FromSlash(c.Path))
		if err := noSymlinkParents(threadDir, p); err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	for _, c := range changes {
		if c.Op == Delete {
			continue
		}
		p := filepath.Join(threadDir, filepath.FromSlash(c.Path))
		if err := noSymlinkParents(threadDir, p); err != nil {
			return err
		}
		if err := copyEntry(runDir, threadDir, c.Path, after.Files[c.Path]); err != nil {
			return err
		}
	}
	return nil
}

func noSymlinkParents(root, p string) error {
	rel, _ := filepath.Rel(root, filepath.Dir(p))
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlinked directory", ErrUnsafe, cur)
		}
	}
	return nil
}

// quarantine stores the validated-as-far-as-possible diff durably, run-scoped.
func quarantine(root, runID, runDir string, meta Meta, after Manifest) (string, error) {
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return "", err
	}
	for _, c := range meta.Changes {
		if c.Op == Delete || SafePath(c.Path) != nil {
			continue
		}
		if err := copyEntry(runDir, filepath.Join(dir, "files"), c.Path, after.Files[c.Path]); err != nil {
			return "", err
		}
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	tmp := filepath.Join(dir, ".meta.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		return "", err
	}
	return dir, nil
}
