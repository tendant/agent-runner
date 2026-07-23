package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/sessionjournal"
)

// recoveryServer builds a real Server (headless — Start never called) over
// sandboxed dirs, with the journal seeded by the caller.
func recoveryServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	// Separate temp dirs for state that background session goroutines keep
	// writing after the test returns — t.TempDir's RemoveAll would race them.
	tmpDir, err := os.MkdirTemp("", "recovery-test-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	logsDir, err := os.MkdirTemp("", "recovery-test-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(logsDir) })

	cfg := config.DefaultConfig()
	cfg.RepoCacheRoot = filepath.Join(dir, "repo-cache")
	cfg.LogsRoot = logsDir
	cfg.TmpRoot = tmpDir
	cfg.StateRoot = filepath.Join(dir, "state")
	cfg.MemoryDir = filepath.Join(dir, "memory")
	cfg.OutputsRoot = filepath.Join(dir, "outputs")
	cfg.UploadsRoot = filepath.Join(dir, "uploads")

	srv := NewServer(cfg)
	if srv.journal == nil {
		t.Fatal("expected the journal to be constructed")
	}
	t.Cleanup(srv.agentManager.Stop)
	t.Cleanup(srv.convManager.Stop)
	return srv
}

// captureHooks records targeted notifications and resume calls.
type captureHooks struct {
	mu      sync.Mutex
	notices map[string][]string // convID → texts
	resumed []string            // "convID/sessionID"
}

func newCaptureHooks() *captureHooks {
	return &captureHooks{notices: make(map[string][]string)}
}

func (c *captureHooks) hooks(source string) map[string]sourceHooks {
	return map[string]sourceHooks{
		source: {
			notify: func(_ context.Context, convID, text string) {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.notices[convID] = append(c.notices[convID], text)
			},
			resume: func(convID, sessionID string) {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.resumed = append(c.resumed, convID+"/"+sessionID)
			},
		},
	}
}

func TestRecovery_RunningSessionFailedAndNotified(t *testing.T) {
	srv := recoveryServer(t)
	cap := newCaptureHooks()

	entry := sessionjournal.Entry{
		SessionID: "agent-run-1",
		Source:    "telegram",
		ConvID:    "555",
		Message:   "build the landing page",
		Status:    "running",
		CreatedAt: time.Now().Add(-time.Minute),
	}
	srv.journal.Write(entry)

	srv.recoverWith([]sessionjournal.Entry{entry}, cap.hooks("telegram"))

	// Session restored under its original ID, terminal, with the interrupted error.
	snap, ok := srv.agentManager.GetSession("agent-run-1")
	if !ok {
		t.Fatal("expected interrupted session restored")
	}
	if snap.Status != agent.SessionStatusFailed || !strings.Contains(snap.Error, "interrupted by server restart") {
		t.Errorf("expected failed/interrupted, got %s %q", snap.Status, snap.Error)
	}

	// Targeted notice reached the conversation; journal entry gone.
	cap.mu.Lock()
	notices := cap.notices["555"]
	cap.mu.Unlock()
	if len(notices) != 1 || !strings.Contains(notices[0], "interrupted by a server restart") {
		t.Errorf("expected targeted interrupted notice, got %v", notices)
	}
	entries, _ := srv.journal.LoadAll()
	if len(entries) != 0 {
		t.Errorf("expected empty journal after recovery, got %d", len(entries))
	}
}

func TestRecovery_QueuedSessionRequeuedAndResumed(t *testing.T) {
	srv := recoveryServer(t)
	cap := newCaptureHooks()

	entry := sessionjournal.Entry{
		SessionID:       "agent-q-1",
		Source:          "stream",
		ConvID:          "c_abc",
		Message:         "summarize the repo",
		MaxIterations:   1,
		MaxTotalSeconds: 30,
		Status:          "queued",
		CreatedAt:       time.Now().Add(-time.Minute),
	}

	srv.recoverWith([]sessionjournal.Entry{entry}, cap.hooks("stream"))

	// Session restored and back in the queue. It dispatches for real in the
	// background (and may quickly fail on the missing CLI in this sandbox),
	// so assert on identity and that it was NOT short-circuited by recovery —
	// not on a transient status.
	snap, ok := srv.agentManager.GetSession("agent-q-1")
	if !ok {
		t.Fatal("expected queued session restored")
	}
	if strings.Contains(snap.Error, "interrupted by server restart") {
		t.Errorf("re-enqueued session must not carry the interrupted error, got %q", snap.Error)
	}

	cap.mu.Lock()
	notices := cap.notices["c_abc"]
	resumed := append([]string(nil), cap.resumed...)
	cap.mu.Unlock()
	if len(notices) != 1 || !strings.Contains(notices[0], "requeued") {
		t.Errorf("expected requeued notice, got %v", notices)
	}
	if len(resumed) != 1 || resumed[0] != "c_abc/agent-q-1" {
		t.Errorf("expected watcher resume for the conversation, got %v", resumed)
	}
}

func TestRecovery_UnknownSourceFallsBackWithoutPanic(t *testing.T) {
	srv := recoveryServer(t)

	entry := sessionjournal.Entry{
		SessionID: "agent-api-1",
		Source:    "api",
		Message:   "an API task",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	srv.journal.Write(entry)

	// No hooks for "api", no notifier wired — must not panic, still restores.
	srv.recoverWith([]sessionjournal.Entry{entry}, map[string]sourceHooks{})

	snap, ok := srv.agentManager.GetSession("agent-api-1")
	if !ok || snap.Status != agent.SessionStatusFailed {
		t.Errorf("expected restored failed session for API source, got ok=%v", ok)
	}
}

func TestRecovery_DuplicateRestoreIsNonFatal(t *testing.T) {
	srv := recoveryServer(t)

	entry := sessionjournal.Entry{
		SessionID: "agent-dup",
		Source:    "api",
		Message:   "task",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	srv.journal.Write(entry)
	// Recover twice — second restore hits the already-exists path.
	srv.recoverWith([]sessionjournal.Entry{entry}, map[string]sourceHooks{})
	srv.recoverWith([]sessionjournal.Entry{entry}, map[string]sourceHooks{})

	if _, ok := srv.agentManager.GetSession("agent-dup"); !ok {
		t.Error("expected session still present after duplicate recovery")
	}
}
