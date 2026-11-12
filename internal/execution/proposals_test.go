package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/proposals"
)

type fakeSink struct {
	on  bool
	got []proposals.Submission
}

func (f *fakeSink) Enabled() bool { return f.on }
func (f *fakeSink) Submit(_ context.Context, sub proposals.Submission) []proposals.Note {
	f.got = append(f.got, sub)
	return []proposals.Note{{Text: "Proposal p-1 (`prompt.md`): x — posted for review in c_1."}, {Text: "_proposals/b.md: dropped: no change", Dropped: true}}
}

func TestCollectProposals(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, proposalsDir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("B hunter2"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a proposal"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "link.md"))

	sink := &fakeSink{on: true}
	h := &Engine{config: &config.Config{}, proposals: sink}
	live := &agent.Session{ID: "s1", Source: "stream"}
	live.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2", "[secret]") })
	h.collectProposals(context.Background(), "s1", live, work)

	if len(sink.got) != 1 {
		t.Fatalf("submissions %d", len(sink.got))
	}
	sub := sink.got[0]
	var names []string
	for _, f := range sub.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a.md,b.md" || sub.SessionID != "s1" || sub.Source != "stream" {
		t.Fatalf("submission %+v (files %v)", sub, names)
	}
	if !sub.Secret("token hunter2") || sub.Secret("nothing here") {
		t.Fatal("the secret check does not use the session's redactor")
	}
	snap := live.Snapshot()
	if len(snap.Notes) != 1 || len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], "dropped") {
		t.Fatalf("notes %v warnings %v", snap.Notes, snap.Warnings)
	}
}

func TestRuntimeInstructionsProposals(t *testing.T) {
	off := runtimeInstructions(runtimeFacts{MemoryDir: "/m"})
	if strings.Contains(off, "_proposals/") {
		t.Error("proposal instructions while proposals are off")
	}
	on := runtimeInstructions(runtimeFacts{MemoryDir: "/m", Proposals: []proposalTarget{{Target: "prompt.md", Base: "3f9c2a1b2c3d"}, {Target: "GOALS.md"}}})
	for _, want := range []string{"_proposals/<short-name>.md", "- `prompt.md`: 3f9c2a1b2c3d", "- `GOALS.md`: new", "skills/<name>/SKILL.md", "propose changes to them in `_proposals/`"} {
		if !strings.Contains(on, want) {
			t.Errorf("lacks %q:\n%s", want, on)
		}
	}
}

// In a runner, proposals are kept for the dispatcher, minus any file that
// holds a secret value (the dispatcher cannot tell one); the dispatcher
// submits them and their notes go on the session's final state.
func TestProposalsDeferredToDispatcher(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, proposalsDir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "a.md"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("B hunter2"), 0o644)

	runner := &Engine{config: &config.Config{}}
	runner.EnableRunnerMode(true)
	if !runner.proposalsOn() {
		t.Fatal("a runner does not tell sessions they may propose")
	}
	live := &agent.Session{ID: "s1", Source: "stream"}
	live.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2", "[secret]") })
	runner.collectProposals(context.Background(), "s1", live, work)
	kept := runner.deferred("s1").Proposals
	if len(kept) != 1 || kept[0].Name != "a.md" {
		t.Fatalf("kept %+v", kept)
	}
	if w := live.Snapshot().Warnings; len(w) != 1 || !strings.Contains(w[0], "b.md") {
		t.Fatalf("warnings %v", w)
	}

	sink := &fakeSink{on: true}
	dispatcher := &Engine{config: &config.Config{}, proposals: sink}
	w := &RunResultWire{SessionID: "s1", Snapshot: &agent.SessionWire{ID: "s1", Source: "stream"},
		Proposals: []ProposalFileWire{{Name: "a.md", Data: []byte("A")}}}
	dispatcher.finishDeferred(context.Background(), "s1", w)
	if len(sink.got) != 1 || len(sink.got[0].Files) != 1 || sink.got[0].Source != "stream" {
		t.Fatalf("submitted %+v", sink.got)
	}
	if len(w.Snapshot.Notes) != 1 || len(w.Snapshot.Warnings) != 1 {
		t.Errorf("notes %v warnings %v", w.Snapshot.Notes, w.Snapshot.Warnings)
	}
}
