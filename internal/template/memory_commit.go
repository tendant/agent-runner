package template

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Each step that changes memory is its own commit (MEMORY_DESIGN.md §5), so
// the history says which session wrote what and one session's notes can be
// undone without the rest. CommitAndPushMemory still commits anything left
// over (an edit outside the runner) and pushes.

// sessionCommitPrefix starts the commit of a session's own memory changes;
// UndoMemorySession finds a session's commits by it.
const sessionCommitPrefix = "memory: session "

// SessionCommitMessage is the commit message for a session's saved notes.
func SessionCommitMessage(sessionID, source string, saved []string) string {
	if source == "" {
		source = "api"
	}
	return fmt.Sprintf("%s%s (%s): saved %s", sessionCommitPrefix, sessionID, source, strings.Join(saved, ", "))
}

// CommitMemory commits everything changed in memoryDir with message, when
// memoryDir is its own git repo (otherwise it does nothing and reports
// false). Nothing is pushed.
func CommitMemory(memoryDir, message string) (bool, error) {
	if !memoryIsRepo(memoryDir) {
		return false, nil
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return commitAll(memoryDir, message)
}

// CommitMemoryFiles commits only the named files (paths relative to
// memoryDir), whatever else in it changed: one step's own changes, not an
// edit someone else left uncommitted. Nothing is pushed.
func CommitMemoryFiles(memoryDir, message string, files ...string) (bool, error) {
	if !memoryIsRepo(memoryDir) || len(files) == 0 {
		return false, nil
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return commitPaths(memoryDir, message, files)
}

// commitPaths commits the current content of files only; the caller holds
// the memory lock.
func commitPaths(memoryDir, message string, files []string) (bool, error) {
	// A path that neither exists nor is tracked (lessons.md before the
	// first lesson) is nothing to commit, and git refuses it outright.
	var keep []string
	for _, f := range files {
		if _, err := os.Lstat(filepath.Join(memoryDir, f)); err == nil {
			keep = append(keep, f)
		} else if out, _ := gitOutput(memoryDir, "ls-files", "--", f); len(strings.TrimSpace(string(out))) > 0 {
			keep = append(keep, f) // tracked and deleted
		}
	}
	if files = keep; len(files) == 0 {
		return false, nil
	}
	args := append([]string{"add", "-A", "--"}, files...)
	if err := gitRunEnv(memoryDir, nil, args...); err != nil {
		return false, err
	}
	out, err := gitOutput(memoryDir, append([]string{"status", "--porcelain", "--"}, files...)...)
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return false, nil
	}
	ensureMemoryIdentity(memoryDir)
	if err := gitRunEnv(memoryDir, nil, append([]string{"commit", "-q", "-m", message, "--only", "--"}, files...)...); err != nil {
		return false, err
	}
	return true, nil
}

func commitAll(memoryDir, message string) (bool, error) {
	if err := gitRunEnv(memoryDir, nil, "add", "-A"); err != nil {
		return false, err
	}
	out, err := gitOutput(memoryDir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return false, nil
	}
	ensureMemoryIdentity(memoryDir)
	if err := gitRunEnv(memoryDir, nil, "commit", "-q", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// ensureMemoryIdentity sets a local commit identity, for hosts (containers)
// with no global git user.
func ensureMemoryIdentity(memoryDir string) {
	_ = gitRunEnv(memoryDir, nil, "config", "user.email", "agent-runner@local")
	_ = gitRunEnv(memoryDir, nil, "config", "user.name", "agent-runner")
}

// MemoryLog is the memory repo's latest n commits, newest first, one line
// each: short hash, date, message.
func MemoryLog(memoryDir string, n int) ([]string, error) {
	if !memoryIsRepo(memoryDir) {
		return nil, fmt.Errorf("the memory dir is not a git repo (run /memory git <remote-url> first)")
	}
	out, err := gitOutput(memoryDir, "log", fmt.Sprintf("-n%d", n), "--date=format:%Y-%m-%d %H:%M", "--format=%h %ad %s")
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\n"), nil
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// UndoMemorySession takes back one session's saved notes (not its daily
// log or curation), as a new commit. A session's change that was a pure
// append (what agents are told to do) is removed where it now sits, even
// after later sessions appended to the same file; anything else is undone
// with git revert, and if later changes touched the same lines nothing
// changes. Returns the commits undone (short hash and message). Push with
// CommitAndPushMemory.
func UndoMemorySession(memoryDir, sessionID string) ([]string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, fmt.Errorf("not a session id: %q", sessionID)
	}
	if !memoryIsRepo(memoryDir) {
		return nil, fmt.Errorf("the memory dir is not a git repo, so there is no history to undo")
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()

	out, err := gitOutput(memoryDir, "log", "--format=%h %s")
	if err != nil {
		return nil, err
	}
	prefix := sessionCommitPrefix + sessionID + " "
	var commits []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		subject := l[strings.Index(l, " ")+1:]
		switch {
		case subject == undoCommitPrefix+sessionID || strings.HasPrefix(subject, `Revert "`+prefix):
			return nil, fmt.Errorf("session %s's notes are already undone", sessionID)
		case strings.HasPrefix(subject, prefix):
			commits = append(commits, l)
		}
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no saved memory from session %s", sessionID)
	}
	if out, _ := gitOutput(memoryDir, "status", "--porcelain"); len(strings.TrimSpace(string(out))) > 0 {
		if _, err := commitAll(memoryDir, "memory: uncommitted changes before undoing session "+sessionID); err != nil {
			return nil, err
		}
	}
	if edits, ok := appendUndo(memoryDir, commits); ok {
		var files []string
		for name, content := range edits {
			files = append(files, name)
			if content == deleteFile {
				os.Remove(filepath.Join(memoryDir, name)) //nolint:errcheck // a file the session created
				continue
			}
			if err := writeMemoryFile(memoryDir, name, content); err != nil {
				return nil, err
			}
		}
		if _, err := commitPaths(memoryDir, undoCommitPrefix+sessionID, files); err != nil {
			return nil, err
		}
		return commits, nil
	}

	head, err := gitOutput(memoryDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	ensureMemoryIdentity(memoryDir)
	for _, c := range commits {
		hash := c[:strings.Index(c, " ")]
		if err := gitRunEnv(memoryDir, nil, "revert", "--no-edit", hash); err != nil {
			_ = gitRunEnv(memoryDir, nil, "revert", "--abort")
			_ = gitRunEnv(memoryDir, nil, "reset", "-q", "--hard", strings.TrimSpace(string(head)))
			return nil, fmt.Errorf("later changes rewrote what %s changed; nothing was undone (edit the files instead)", c)
		}
	}
	return commits, nil
}

const undoCommitPrefix = "memory: undo session "

// deleteFile marks, in appendUndo's edits, a file the session created and
// undoing it empties: removed, not left empty.
const deleteFile = "\x00delete"

// appendUndo works out, for commits that only appended to files, each file's
// content with those appends removed from where they now are. ok is false
// if any change was not a pure append or its text is no longer there.
func appendUndo(memoryDir string, commits []string) (map[string]string, bool) {
	edits := map[string]string{}
	for _, c := range commits { // newest first
		hash := c[:strings.Index(c, " ")]
		names, err := gitOutput(memoryDir, "show", "--name-only", "--format=", hash)
		if err != nil {
			return nil, false
		}
		for _, name := range strings.Split(strings.TrimSpace(string(names)), "\n") {
			if name == "" {
				continue
			}
			before, berr := gitOutput(memoryDir, "show", hash+"^:"+name)
			created := berr != nil // the commit created the file
			after, err := gitOutput(memoryDir, "show", hash+":"+name)
			if err != nil || !strings.HasPrefix(string(after), string(before)) || len(after) == len(before) {
				return nil, false
			}
			added := string(after[len(before):])
			cur, seen := edits[name]
			if !seen {
				b, err := os.ReadFile(filepath.Join(memoryDir, name))
				if err != nil {
					return nil, false
				}
				cur = string(b)
			}
			// Where the session appended it: later sessions only appended
			// after it (commits are undone newest first). Elsewhere only if
			// earlier text was edited since: then its first occurrence.
			i := len(before)
			if i > len(cur) || cur[i:min(len(cur), i+len(added))] != added {
				if i = strings.Index(cur, added); i < 0 {
					return nil, false
				}
			}
			edits[name] = cur[:i] + cur[i+len(added):]
			if created && edits[name] == "" {
				edits[name] = deleteFile
			}
		}
	}
	return edits, true
}

// memoryIsRepo reports whether memoryDir is its own git repo (/memory git).
func memoryIsRepo(memoryDir string) bool {
	if memoryDir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(memoryDir, ".git"))
	return err == nil && fi.IsDir()
}

// WriteInstructions writes an instructions file (agent.md, prompt.md) for
// the operator, under the memory lock, and when it lives in a memory git
// repo commits it alone with message: an operator's edit is never swept
// into a session's commit, so undoing a session cannot take it back.
func WriteInstructions(memoryDir, path, content, message string) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	if err := writeMemoryFile(filepath.Dir(path), filepath.Base(path), content); err != nil {
		return err
	}
	return commitInstructions(memoryDir, path, message)
}

// CommitInstructions commits an instructions file something else wrote
// (the startup upgrade of an unedited default), when it is in a memory git
// repo.
func CommitInstructions(memoryDir, path, message string) error {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	return commitInstructions(memoryDir, path, message)
}

func commitInstructions(memoryDir, path, message string) error {
	if !memoryIsRepo(memoryDir) {
		return nil
	}
	absMem, _ := filepath.Abs(memoryDir)
	absPath, _ := filepath.Abs(path)
	rel, err := filepath.Rel(absMem, absPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil // outside the memory dir (AGENT_SYSTEM_PROMPT elsewhere)
	}
	_, err = commitPaths(memoryDir, message, []string{rel})
	return err
}
