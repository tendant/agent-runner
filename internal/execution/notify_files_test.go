package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
)

type notifyDeps struct {
	promptDeps
	n Notifier
}

func (d notifyDeps) Notifier() Notifier { return d.n }

type recNotifier struct {
	text  string
	files []string
}

func (r *recNotifier) SendNotification(_ context.Context, m string) error { r.text = m; return nil }

type recFileNotifier struct{ recNotifier }

func (r *recFileNotifier) SendNotificationFiles(_ context.Context, m string, files []agent.OutputFile) error {
	r.text = m
	for _, f := range files {
		r.files = append(r.files, f.Name)
	}
	return nil
}

// A scheduled session's _send/ files go with its result: attached where the
// notifier can, named where it cannot.
func TestNotifySessionResultSendsOutputFiles(t *testing.T) {
	snap := &agent.Session{ID: "s1", Source: "runner", Status: agent.SessionStatusCompleted,
		Iterations:  []agent.IterationResult{{Output: "watchlist: 4 names"}},
		OutputFiles: []agent.OutputFile{{Name: "watchlist-2026-11-09.md", Data: []byte("# r")}}}

	fn := &recFileNotifier{}
	(&Engine{config: &config.Config{}, deps: notifyDeps{n: fn}}).notifySessionResult(snap)
	if fn.text != "watchlist: 4 names" || len(fn.files) != 1 || fn.files[0] != "watchlist-2026-11-09.md" {
		t.Errorf("file notifier got %q %v", fn.text, fn.files)
	}

	plain := &recNotifier{}
	(&Engine{config: &config.Config{}, deps: notifyDeps{n: plain}}).notifySessionResult(snap)
	if !strings.HasPrefix(plain.text, "watchlist: 4 names") || !strings.Contains(plain.text, "watchlist-2026-11-09.md") {
		t.Errorf("plain notifier got %q", plain.text)
	}
}
