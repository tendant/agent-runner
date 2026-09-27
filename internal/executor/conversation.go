package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Conversation makes a backend's conversation outlive one Session: the
// turns of a multi-turn task each start a new Session with the same
// Conversation and continue where the last one stopped. Backends that can
// resume keep their own reference under Dir; the others ignore it and rely
// on the prompt alone.
type Conversation struct {
	Dir string
}

// ContextRetainer is implemented by sessions that report whether the
// conversation carries over between Prompt calls. The engine then sends
// incremental prompts, as it does for persistent backends. Sessions that
// don't implement it retain context iff their backend is Persistent.
type ContextRetainer interface {
	RetainsContext() bool
}

// ResumingExecutor is implemented by one-shot executors that can continue a
// saved conversation. The returned ref is the one to continue next time.
// ErrResumeFailed means req.Ref could not be resumed and nothing ran.
type ResumingExecutor interface {
	// ResumeKind names the reference format ("claude", "codex"), so a
	// reference saved by one CLI is never handed to another after a config
	// change.
	ResumeKind() string
	ExecuteResuming(ctx context.Context, workspacePath string, req ResumeRequest) (result *ExecutionResult, newRef string, err error)
}

// ResumeRequest is one prompt in a resumable conversation.
type ResumeRequest struct {
	SystemPrompt string
	Instruction  string
	Ref          string // conversation to continue; "" starts a new one
	// Continuation marks an incremental prompt: the conversation already
	// holds the instructions (SystemPrompt repeats the previous one), so a
	// CLI that has to inline the system prompt into the message skips it.
	Continuation bool
	OnEvent      func(EventKind, string)
}

// ErrResumeFailed reports that a saved conversation reference was rejected
// (missing or unreadable session). Callers start a fresh conversation.
var ErrResumeFailed = errors.New("backend could not resume the conversation")

// refFile is where a backend keeps its conversation reference.
func (c *Conversation) refFile(backend string) string {
	return filepath.Join(c.Dir, backend+"-session")
}

// loadRef returns the saved reference for backend, or "".
func (c *Conversation) loadRef(backend string) string {
	data, err := os.ReadFile(c.refFile(backend))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveRef records the reference for backend; "" clears it.
func (c *Conversation) saveRef(backend, ref string) {
	path := c.refFile(backend)
	if ref == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		slog.Warn("conversation: cannot create dir", "dir", c.Dir, "error", err)
		return
	}
	if err := os.WriteFile(path, []byte(ref+"\n"), 0o644); err != nil {
		slog.Warn("conversation: cannot save reference", "path", path, "error", err)
	}
}

// piSession returns pi's session directory and ID for this conversation,
// creating the ID on first use (pi creates the session file itself).
func (c *Conversation) piSession() (dir, id string) {
	id = c.loadRef("pi")
	if id == "" {
		id = uuid.NewString()
		c.saveRef("pi", id)
	}
	return filepath.Join(c.Dir, "pi"), id
}

// discardPi moves pi's session aside so the next start begins a fresh
// conversation (used when a saved session makes pi die on startup).
func (c *Conversation) discardPi() {
	dir := filepath.Join(c.Dir, "pi")
	if _, err := os.Stat(dir); err == nil {
		_ = os.Rename(dir, fmt.Sprintf("%s.discarded-%d", dir, time.Now().Unix()))
	}
	c.saveRef("pi", "")
}
