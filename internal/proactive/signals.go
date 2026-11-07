package proactive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

// Signal is one fact a probe found, runner-side and without a model call.
type Signal struct {
	Key  string // stable while the fact holds: the cooldown's key
	Kind string // repo-ci, repo-commits
	Text string // for the report: data, not instructions
}

// Prober runs the policy's signal probes on the watched repos. heads is
// what repo-commits saw last (repo -> commit), updated in place.
type Prober interface {
	Probe(ctx context.Context, kinds, names []string, heads map[string]string) []Signal
}

// RepoSignals probes the registered repos (internal/repos): repo-ci asks
// GitHub for the latest CI result on a repo's branch, with the repo's login
// when it has a token; repo-commits fetches the bare cache and lists commits
// that are new since the last check. It never runs anything from a repo.
type RepoSignals struct {
	Repos     func() ([]repos.Repo, error)
	CacheRoot string
	Secrets   *agentsecrets.Store
	GitHubAPI string       // default https://api.github.com
	HTTP      *http.Client // default: a 15s timeout
}

// probeTimeout bounds one repo's probe.
const probeTimeout = 30 * time.Second

func (r *RepoSignals) Probe(ctx context.Context, kinds, names []string, heads map[string]string) []Signal {
	if r == nil || r.Repos == nil || len(kinds) == 0 {
		return nil
	}
	list, err := r.Repos()
	if err != nil {
		slog.Warn("proactive: repo registry unreadable; no repo signals", "error", err)
		return nil
	}
	var out []Signal
	for _, rp := range list {
		if len(names) > 0 && !slices.Contains(names, rp.Name) {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		if slices.Contains(kinds, SignalCommits) {
			if sig, err := r.commits(pctx, rp, heads); err != nil {
				slog.Warn("proactive: repo-commits probe failed", "repo", rp.Name, "error", err)
			} else if sig != nil {
				out = append(out, *sig)
			}
		}
		if slices.Contains(kinds, SignalCI) {
			if sig, err := r.ci(pctx, rp); err != nil {
				slog.Warn("proactive: repo-ci probe failed", "repo", rp.Name, "error", err)
			} else if sig != nil {
				out = append(out, *sig)
			}
		}
		cancel()
	}
	return out
}

// commits fetches rp's cache and reports the commits on its branch since
// the last check. The first check only records where the branch is.
func (r *RepoSignals) commits(ctx context.Context, rp repos.Repo, heads map[string]string) (*Signal, error) {
	if err := repos.Sync(ctx, rp, r.CacheRoot, r.Secrets); err != nil {
		return nil, err
	}
	cache := repos.CachePath(r.CacheRoot, rp.Name)
	ref := "HEAD"
	if rp.Branch != "" {
		ref = "refs/heads/" + rp.Branch
	}
	head, err := git(ctx, cache, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("branch %s: %w", ref, err)
	}
	last := heads[rp.Name]
	heads[rp.Name] = head
	if last == "" || last == head {
		return nil, nil
	}
	logOut, err := git(ctx, cache, "log", "--no-merges", "--format=%h %an: %s", "-n", "20", last+".."+head)
	if err != nil {
		// The old head is gone (a force-push): show the latest commits.
		logOut, _ = git(ctx, cache, "log", "--no-merges", "--format=%h %an: %s", "-n", "10", head)
	}
	lines := strings.Split(strings.TrimSpace(logOut), "\n")
	branch := rp.Branch
	if branch == "" {
		branch = "its default branch"
	}
	text := fmt.Sprintf("%d new commit(s) on %s (%s), now at %s:\n", len(lines), rp.Name, branch, head[:min(len(head), 12)])
	for _, l := range lines {
		text += "  - " + oneLine(l, 160) + "\n"
	}
	return &Signal{Key: "commits:" + rp.Name + ":" + head, Kind: SignalCommits, Text: strings.TrimRight(text, "\n")}, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var githubRepo = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com/)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ci reports a failing CI result on rp's branch, from GitHub's check runs
// and commit statuses. Nothing while checks are still running or all pass;
// nothing for a repo that is not on GitHub.
func (r *RepoSignals) ci(ctx context.Context, rp repos.Repo) (*Signal, error) {
	m := githubRepo.FindStringSubmatch(rp.URL)
	if m == nil {
		return nil, nil
	}
	owner, name := m[1], m[2]
	token := r.token(rp)
	branch := rp.Branch
	if branch == "" {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := r.get(ctx, token, "/repos/"+owner+"/"+name, &info); err != nil {
			return nil, err
		}
		branch = info.DefaultBranch
	}
	ref := url.PathEscape(branch)
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
		} `json:"check_runs"`
	}
	if err := r.get(ctx, token, "/repos/"+owner+"/"+name+"/commits/"+ref+"/check-runs?per_page=100", &runs); err != nil {
		return nil, err
	}
	var status struct {
		SHA      string `json:"sha"`
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := r.get(ctx, token, "/repos/"+owner+"/"+name+"/commits/"+ref+"/status", &status); err != nil {
		return nil, err
	}
	sha := status.SHA
	var failed []string
	pending := false
	for _, c := range runs.CheckRuns {
		if sha == "" {
			sha = c.HeadSHA
		}
		switch {
		case c.Status != "completed":
			pending = true
		case slices.Contains([]string{"failure", "timed_out", "action_required", "startup_failure"}, c.Conclusion):
			failed = append(failed, c.Name+" ("+c.Conclusion+")")
		}
	}
	for _, s := range status.Statuses {
		switch s.State {
		case "pending":
			pending = true
		case "failure", "error":
			failed = append(failed, s.Context+" ("+s.State+")")
		}
	}
	if len(failed) == 0 || pending || sha == "" {
		return nil, nil
	}
	text := fmt.Sprintf("CI is failing on %s (%s) at %s: %s", rp.Name, branch, sha[:min(len(sha), 12)], oneLine(strings.Join(failed, ", "), 400))
	return &Signal{Key: "ci:" + rp.Name + ":" + sha, Kind: SignalCI, Text: text}, nil
}

// token is the repo's login when it is a token (not an ssh key).
func (r *RepoSignals) token(rp repos.Repo) string {
	if rp.Secret == "" || rp.IsSSH() || r.Secrets == nil {
		return ""
	}
	list, err := r.Secrets.List()
	if err != nil {
		return ""
	}
	for _, s := range list {
		if s.Name == rp.Secret {
			return s.Value
		}
	}
	return ""
}

func (r *RepoSignals) get(ctx context.Context, token, path string, v any) error {
	api := r.GitHubAPI
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agent-runner")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck
		return fmt.Errorf("GitHub %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}
