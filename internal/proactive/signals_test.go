package proactive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/repos"
)

func TestRepoCI(t *testing.T) {
	var auth string
	runs := `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure","head_sha":"abc123def456"},{"name":"lint","status":"completed","conclusion":"success"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/repos/acme/app":
			w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/acme/app/commits/main/check-runs":
			w.Write([]byte(runs))
		case "/repos/acme/app/commits/main/status":
			w.Write([]byte(`{"sha":"abc123def456","statuses":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rs := &RepoSignals{GitHubAPI: srv.URL, Repos: func() ([]repos.Repo, error) {
		return []repos.Repo{{Name: "app", URL: "https://github.com/acme/app.git"}, {Name: "other", URL: "https://gitlab.com/x/y.git"}}, nil
	}}
	sigs := rs.Probe(context.Background(), []string{SignalCI}, nil, map[string]string{})
	if len(sigs) != 1 || sigs[0].Key != "ci:app:abc123def456" || !strings.Contains(sigs[0].Text, "test (failure)") || strings.Contains(sigs[0].Text, "lint") {
		t.Fatalf("signals %+v", sigs)
	}
	if auth != "" {
		t.Fatalf("a public repo sent a login: %q", auth)
	}
	// Still running: no verdict yet.
	runs = `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure","head_sha":"abc"},{"name":"e2e","status":"in_progress"}]}`
	if sigs := rs.Probe(context.Background(), []string{SignalCI}, []string{"app"}, nil); len(sigs) != 0 {
		t.Fatalf("pending gave %+v", sigs)
	}
	runs = `{"check_runs":[{"name":"test","status":"completed","conclusion":"success","head_sha":"abc"}]}`
	if sigs := rs.Probe(context.Background(), []string{SignalCI}, nil, nil); len(sigs) != 0 {
		t.Fatalf("green gave %+v", sigs)
	}
}

func TestRepoCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	remote := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", remote}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=a@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "first")
	rs := &RepoSignals{CacheRoot: filepath.Join(t.TempDir(), "cache"), Repos: func() ([]repos.Repo, error) {
		return []repos.Repo{{Name: "app", URL: remote}}, nil
	}}
	heads := map[string]string{}
	if sigs := rs.Probe(context.Background(), []string{SignalCommits}, nil, heads); len(sigs) != 0 || heads["app"] == "" {
		t.Fatalf("first look: %+v heads %v", sigs, heads)
	}
	run("commit", "-q", "--allow-empty", "-m", "fix the flaky test")
	run("commit", "-q", "--allow-empty", "-m", "bump deps")
	sigs := rs.Probe(context.Background(), []string{SignalCommits}, nil, heads)
	if len(sigs) != 1 || !strings.Contains(sigs[0].Text, "2 new commit(s) on app") || !strings.Contains(sigs[0].Text, "Ann: fix the flaky test") || strings.Contains(sigs[0].Text, "first") {
		t.Fatalf("signals %+v", sigs)
	}
	if sigs := rs.Probe(context.Background(), []string{SignalCommits}, nil, heads); len(sigs) != 0 {
		t.Fatalf("nothing new gave %+v", sigs)
	}
}
