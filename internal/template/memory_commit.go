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

// UndoMemorySession reverts the commits of one session's saved notes, newest
// first, as new commits; the daily log and curation commits stay. If a later
// change touched the same lines the revert is abandoned and nothing changes.
// Returns the commits reverted (short hash and message). Push with
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

	out, err := gitOutput(memoryDir, "log", "--fixed-strings", "--grep="+sessionCommitPrefix+sessionID+" ", "--format=%h %s")
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && strings.HasPrefix(l[strings.Index(l, " ")+1:], sessionCommitPrefix+sessionID+" ") {
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
			return nil, fmt.Errorf("later changes touched the same lines as %s; nothing was undone (edit the files instead)", c)
		}
	}
	return commits, nil
}

// memoryIsRepo reports whether memoryDir is its own git repo (/memory git).
func memoryIsRepo(memoryDir string) bool {
	if memoryDir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(memoryDir, ".git"))
	return err == nil && fi.IsDir()
}
