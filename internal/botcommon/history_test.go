package botcommon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatHistory_KeepsNewest(t *testing.T) {
	msgs := []HistoryMessage{
		{Speaker: "User a", Content: strings.Repeat("old ", 100)},
		{Speaker: "You (the assistant)", Content: "middle"},
		{Speaker: "User a", Content: "newest"},
	}
	got := FormatHistory(msgs, 60)
	if !strings.HasPrefix(got, "[1 earlier messages omitted]") || !strings.HasSuffix(got, "User a: newest") || !strings.Contains(got, "middle") {
		t.Fatalf("got %q", got)
	}
	if FormatHistory(nil, 100) != "" {
		t.Fatal("empty history should render empty")
	}
	long := strings.Repeat("x", 3000) + " [Image: /u/shot.png]"
	if got := FormatHistory([]HistoryMessage{{Speaker: "U", Content: long}}, 10000); !strings.Contains(got, "[Image: /u/shot.png]") || len(got) > 2100 {
		t.Fatalf("long message not capped with its attachment kept (%d chars)", len(got))
	}
}

// A new task's prompt is built from the server's thread history, which has
// what the bot itself forgot; a continuing turn gets the latest messages
// next to its context block.
func TestRunTurn_UsesServerHistory(t *testing.T) {
	f, ts := newTaskFixture(t)
	history := []HistoryMessage{
		{Speaker: "User 6a31", Content: "the export is broken"},
		{Speaker: "User e426", Content: "it started after Tuesday's deploy"},
		{Speaker: "You (the assistant)", Content: "earlier reply", Self: true},
		{Speaker: "User 6a31", Content: "please fix it"}, // the current message
	}
	f.engine.History = func(context.Context, string) ([]HistoryMessage, error) { return history, nil }

	f.conv.AddMessage("user", "please fix it")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	_, msgs := ts.counts()
	if len(msgs) != 1 {
		t.Fatalf("started %d turns", len(msgs))
	}
	for _, want := range []string{"## Conversation History", "User e426: it started after Tuesday's deploy", "You (the assistant): earlier reply", "## Current Request\n\nplease fix it"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("new task prompt missing %q:\n%s", want, msgs[0])
		}
	}
	if strings.Count(msgs[0], "please fix it") != 1 {
		t.Errorf("the current message is repeated in the history:\n%s", msgs[0])
	}

	// Feedback on the done task: a continuing turn.
	history = append(history,
		HistoryMessage{Speaker: "You (the assistant)", Content: "fixed the export", Self: true},
		HistoryMessage{Speaker: "User e426", Content: "see the attached screenshot (attached 1 file(s))"},
		HistoryMessage{Speaker: "User 6a31", Content: "still broken on QA"},
	)
	conv := f.mgr.GetOrCreate("conv-1")
	conv.AddMessage("user", "still broken on QA")
	f.engine.resumeTask(context.Background(), "conv-1", conv, true)
	f.engine.WG.Wait()
	_, msgs = ts.counts()
	if len(msgs) != 2 {
		t.Fatalf("started %d turns, want 2", len(msgs))
	}
	if !strings.HasPrefix(msgs[1], "## Recent Thread Messages") || !strings.Contains(msgs[1], "User e426: see the attached screenshot") || !strings.Contains(msgs[1], "## Task") {
		t.Fatalf("continuing turn lacks the recent messages:\n%s", msgs[1])
	}
}

// Without a server history (other transports, or a failed fetch) the bot's
// own record is used, as before.
func TestRunTurn_FallsBackToOwnHistory(t *testing.T) {
	f, ts := newTaskFixture(t)
	f.engine.History = func(context.Context, string) ([]HistoryMessage, error) { return nil, context.DeadlineExceeded }
	f.conv.AddMessage("user", "first")
	f.conv.AddMessage("assistant", "answer")
	f.conv.AddMessage("user", "second")
	f.engine.HandleConfirmation(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()
	_, msgs := ts.counts()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "User: first") {
		t.Fatalf("fallback history missing: %q", msgs)
	}
}

// An alert triage executes under the operator's policy starts a run told so
// by the runner, with the policy; a person's go-ahead needs no such note.
func TestAlertExecute_CarriesOperatorAuthorization(t *testing.T) {
	f := newEngineFixture(t, `{"action":"execute","message":"Fixing it."}`)
	policy := filepath.Join(t.TempDir(), "ALERTS.md")
	os.WriteFile(policy, []byte("CI failing on main: fix and push without asking."), 0o600)
	f.engine.Analyzer.SetAlertPolicyPath(policy)

	f.conv.AddAlertMessage("[Automated alert from webhook \"ci\"] build on main is red")
	f.engine.HandleAnalysis(context.Background(), "conv-1", f.conv)
	f.engine.WG.Wait()

	_, msgs := f.starter.counts()
	if len(msgs) != 1 {
		t.Fatalf("started %d runs, want 1", len(msgs))
	}
	if !strings.HasPrefix(msgs[0], "## Operator Authorization") || !strings.Contains(msgs[0], "fix and push without asking") {
		t.Fatalf("run lacks the operator's authorization:\n%s", msgs[0])
	}

	// The note is used once: a person's next request runs without it.
	conv := f.mgr.GetOrCreate("conv-1")
	conv.AddMessage("user", "also update the changelog")
	f.engine.HandleConfirmation(context.Background(), "conv-1", conv)
	f.engine.WG.Wait()
	_, msgs = f.starter.counts()
	if len(msgs) != 2 || strings.Contains(msgs[1], "Operator Authorization") {
		t.Fatalf("authorization leaked into a later run: %q", msgs)
	}
}
