package proactive

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/repos"
	"github.com/agent-runner/agent-runner/internal/thread"
	"github.com/agent-runner/agent-runner/internal/trust"
)

type fakeReverts struct {
	calls int
	found []Revert
}

func (f *fakeReverts) Reverted(_ context.Context, repo string, _ time.Time, commits []Commit) ([]Revert, error) {
	f.calls++
	return f.found, nil
}

type fakeJudge struct {
	mu    sync.Mutex
	wrong bool
	seen  []string
	done  chan struct{}
}

func (j *fakeJudge) JudgeReply(_ context.Context, work, reply string) (bool, string, error) {
	j.mu.Lock()
	j.seen = append(j.seen, work+" | "+reply)
	j.mu.Unlock()
	defer func() { j.done <- struct{}{} }()
	return j.wrong, "it touched prod", nil
}

// startNotifyRun starts one ci-fix run at notify and lets it finish clean,
// having pushed a commit.
func startNotifyRun(t *testing.T) (*fixture, *trust.Store) {
	t.Helper()
	f, ts, _ := newClassFixture(t)
	f.triager.res = thread.AnalysisResult{Action: "execute", Class: "ci-fix", Message: "fix the red test"}
	if o := f.svc.Tick(context.Background(), false); o.Result != "execute" {
		t.Fatalf("run: %+v", o)
	}
	f.sessions = []Session{{ID: "run-1", Status: "completed", Ended: f.now, Pushed: []Commit{{Repo: "app", SHA: "abc1234def", Subject: "fix the red test"}}}}
	f.svc.followRuns(context.Background())
	return f, ts
}

func TestRevertDemotes(t *testing.T) {
	f, ts := startNotifyRun(t)
	if st := f.svc.load(); len(st.Done["run-1"].Commits) != 1 || st.Done["run-1"].Thread != "thread-1" {
		t.Fatalf("run record %+v", st.Done["run-1"])
	}
	rv := &fakeReverts{}
	f.svc.cfg.Reverts = rv
	ctx := context.Background()

	f.svc.checkReverts(ctx, false)
	if rv.calls != 1 || ts.Level("ci-fix", trust.Notify) != trust.Notify {
		t.Fatalf("nothing reverted: calls %d level %s", rv.calls, ts.Level("ci-fix", trust.Notify))
	}
	// Throttled: not again within 30 minutes.
	rv.found = []Revert{{Commit: Commit{Repo: "app", SHA: "abc1234def", Subject: "fix the red test"}, By: "fff9999"}}
	f.svc.checkReverts(ctx, false)
	if rv.calls != 1 {
		t.Fatal("looked again within 30 minutes")
	}
	f.now = f.now.Add(31 * time.Minute)
	f.svc.checkReverts(ctx, false)
	if l := ts.Level("ci-fix", trust.Notify); l != trust.Propose {
		t.Fatalf("after the revert: %s", l)
	}
	last := f.chat.posts[len(f.chat.posts)-1].message
	if !strings.Contains(last, "was reverted in app by fff9999") {
		t.Fatalf("notice:\n%s", last)
	}
	// Once per run: neither another look nor an undo demotes it again.
	f.now = f.now.Add(31 * time.Minute)
	f.svc.checkReverts(ctx, false)
	if f.svc.SessionUndone(ctx, "run-1") != "" || ts.Level("ci-fix", trust.Notify) != trust.Propose {
		t.Fatalf("demoted twice: %s", ts.Level("ci-fix", trust.Notify))
	}
	// /proactive tick looks at once, throttle or not.
	rv.calls = 0
	f.svc.Tick(ctx, true)
	if rv.calls != 0 {
		t.Fatal("looked at a run that already demoted its class")
	}
	// After 7 days a run's commits are no longer watched.
	rv.calls = 0
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.svc.checkReverts(ctx, false)
	if rv.calls != 0 {
		t.Fatal("an old run was looked at")
	}
}

func TestWrongReplyDemotes(t *testing.T) {
	f, ts := startNotifyRun(t)
	j := &fakeJudge{done: make(chan struct{}, 4)}
	f.svc.cfg.Judge = j

	// Another thread: not judged.
	if _, handled := f.svc.ThreadReply("thread-9", "u", true, "this is wrong"); handled || len(j.seen) != 0 {
		t.Fatal("judged a reply in another thread")
	}
	// A reply that is not a rejection: judged, nothing happens.
	if _, handled := f.svc.ThreadReply("thread-1", "u", false, "thanks, what about the lint?"); handled {
		t.Fatal("consumed the reply")
	}
	<-j.done
	if ts.Level("ci-fix", trust.Notify) != trust.Notify {
		t.Fatal("demoted on a thank-you")
	}
	if !strings.HasPrefix(j.seen[0], "fix the red test | ") {
		t.Fatalf("judge saw %q", j.seen[0])
	}
	// "This was wrong": demoted, once.
	j.wrong = true
	f.svc.ThreadReply("thread-1", "u", false, "no, you broke prod")
	<-j.done
	deadline := time.Now().Add(5 * time.Second)
	for ts.Level("ci-fix", trust.Notify) != trust.Propose && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if l := ts.Level("ci-fix", trust.Notify); l != trust.Propose {
		t.Fatalf("after a rejection: %s", l)
	}
	f.svc.ThreadReply("thread-1", "u", false, "and again")
	time.Sleep(50 * time.Millisecond)
	if c, _ := ts.Get("ci-fix"); c.History.Demoted != 1 {
		t.Fatalf("demoted %d times", c.History.Demoted)
	}
}

func TestRepoReverted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	remote := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", remote}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "base")
	os.WriteFile(filepath.Join(remote, "f"), []byte("x"), 0o644)
	run("add", "f")
	run("commit", "-q", "-m", "agent fix")
	sha := run("rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "unrelated")
	rs := &RepoSignals{CacheRoot: filepath.Join(t.TempDir(), "cache"), Repos: func() ([]repos.Repo, error) {
		return []repos.Repo{{Name: "app", URL: remote}}, nil
	}}
	commits := []Commit{{Repo: "app", SHA: sha, Subject: "agent fix"}}
	since := time.Now().Add(-time.Hour)
	if got, err := rs.Reverted(context.Background(), "app", since, commits); err != nil || len(got) != 0 {
		t.Fatalf("before the revert: %+v %v", got, err)
	}
	run("revert", "--no-edit", sha)
	got, err := rs.Reverted(context.Background(), "app", since, commits)
	if err != nil || len(got) != 1 || got[0].Commit.SHA != sha || got[0].By == "" {
		t.Fatalf("after git revert: %+v %v", got, err)
	}
	// Matched by subject when the hash it reverts is not the one recorded.
	other := []Commit{{Repo: "app", SHA: "0000000000", Subject: "agent fix"}}
	if got, _ := rs.Reverted(context.Background(), "app", since, other); len(got) != 1 {
		t.Fatalf("by subject: %+v", got)
	}
	if got, _ := rs.Reverted(context.Background(), "gone", since, commits); len(got) != 0 {
		t.Fatal("an unregistered repo")
	}
}

func TestForcedTickChecksRevertsAtOnce(t *testing.T) {
	f, ts := startNotifyRun(t)
	rv := &fakeReverts{}
	f.svc.cfg.Reverts = rv
	f.svc.checkReverts(context.Background(), false) // the scheduled look: nothing yet
	rv.found = []Revert{{Commit: Commit{Repo: "app", SHA: "abc1234def", Subject: "fix the red test"}, By: "fff9999"}}
	f.svc.Tick(context.Background(), true) // well within 30 minutes
	if rv.calls != 2 || ts.Level("ci-fix", trust.Notify) != trust.Propose {
		t.Fatalf("calls %d level %s", rv.calls, ts.Level("ci-fix", trust.Notify))
	}
}
