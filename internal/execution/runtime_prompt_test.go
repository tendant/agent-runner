package execution

import (
	"context"
	"github.com/agent-runner/agent-runner/internal/executor"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

func TestRuntimeInstructions(t *testing.T) {
	host := runtimeInstructions(runtimeFacts{MemoryDir: "/data/memory"})
	box := runtimeInstructions(runtimeFacts{Sandboxed: true, MemoryDir: "/state/home/s1/memory", MemoryCopy: true})

	for _, want := range []string{"agent-runner pushes", "/repo add", "/data/memory", "several people", "do not edit them", "_send/"} {
		if !strings.Contains(host, want) {
			t.Errorf("unsandboxed: missing %q:\n%s", want, host)
		}
	}
	if strings.Contains(host, "your own copy") {
		t.Errorf("unsandboxed sessions must not get the sandbox's rules:\n%s", host)
	}
	for _, want := range []string{"do not `git push`", "agent-runner pushes", "/state/home/s1/memory", "your own copy", "never yours to use"} {
		if !strings.Contains(box, want) {
			t.Errorf("sandboxed: missing %q:\n%s", want, box)
		}
	}
	repoNote := runtimeInstructions(runtimeFacts{Repos: []repos.Repo{{Name: "oam-server", URL: "https://lei@gitee.com/o/oam-server.git", Branch: "develop", Push: true}, {Name: "docs", URL: "https://github.com/o/docs.git"}}})
	for _, want := range []string{"- `oam-server/`: https://lei@gitee.com/o/oam-server.git, on develop, pushed for you", "- `docs/`: https://github.com/o/docs.git, on its default branch, read-only"} {
		if !strings.Contains(repoNote, want) {
			t.Errorf("repo list: missing %q:\n%s", want, repoNote)
		}
	}
	if strings.Contains(runtimeInstructions(runtimeFacts{}), "### Memory") {
		t.Error("no memory section without a memory dir")
	}
}

// The prompt names the agent user's secrets, never their values.
func TestSessionPromptNamesSecrets(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	if err := agentsecrets.New(h.config.SecretsFile).Set("GITEE_TOKEN", "tok-secret-value", "Gitee API for oam-server"); err != nil {
		t.Fatal(err)
	}
	var prompt string
	h.executor = promptExecutor{fn: func(p string) {
		if prompt == "" {
			prompt = p
		}
	}}
	session, _ := h.agentManager.CreateSession("check", nil, "test", "", 1, 30)
	h.ExecuteAgent(session)
	if !strings.Contains(prompt, "`$GITEE_TOKEN`: Gitee API for oam-server") {
		t.Errorf("secret not named in the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "tok-secret-value") {
		t.Error("a secret's value must never be in the prompt")
	}
}

// A secret value the agent echoes is redacted in what the session records
// (and so in chat, the API and the audit log).
func TestSessionRedactsSecretValues(t *testing.T) {
	env := setupTestEnv(t)
	h := env.handlers
	h.config.Agent.CLI = "pi"
	h.config.Agent.PlannerEnabled = false
	h.config.SecretsFile = filepath.Join(t.TempDir(), "secrets.json")
	agentsecrets.New(h.config.SecretsFile).Set("GITEE_TOKEN", "gitee-secret-value-1", "")
	h.executor = echoExecutor{text: "the token is gitee-secret-value-1"}
	session, _ := h.agentManager.CreateSession("show it", nil, "test", "", 1, 30)
	h.ExecuteAgent(session)
	for _, it := range session.Snapshot().Iterations {
		if strings.Contains(it.Output, "gitee-secret-value-1") || !strings.Contains(it.Output, "[redacted:GITEE_TOKEN]") {
			t.Errorf("output not redacted: %q", it.Output)
		}
	}
}

type echoExecutor struct{ text string }

func (e echoExecutor) Execute(context.Context, string, string) (*executor.ExecutionResult, error) {
	return &executor.ExecutionResult{Output: e.text}, nil
}
func (e echoExecutor) ExecuteWithSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, error) {
	return e.Execute(ctx, w, in)
}
func (e echoExecutor) ExecuteWithLog(ctx context.Context, w, in string) (*executor.ExecutionResult, string, error) {
	r, err := e.Execute(ctx, w, in)
	return r, "", err
}
func (e echoExecutor) ExecuteWithLogAndSystemPrompt(ctx context.Context, w, _, in string) (*executor.ExecutionResult, string, error) {
	r, err := e.Execute(ctx, w, in)
	return r, "", err
}

// The push command the agent is given works as written: it pushes the
// branch (so the runner finds nothing left to push), and its credential
// helper answers with the repo's login from the environment.
func TestSelfPushCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, url := range []string{"https://lei@gitee.com/o/app.git", "git@gitee.com:o/app.git"} {
		base := t.TempDir()
		remote := filepath.Join(base, "remote.git")
		run := func(dir string, args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return string(out)
		}
		run(base, "init", "-q", "--bare", "-b", "main", remote)
		ws := filepath.Join(base, "workspace")
		os.MkdirAll(ws, 0o755)
		run(ws, "clone", "-q", remote, "app")
		app := filepath.Join(ws, "app")
		run(app, "commit", "-q", "--allow-empty", "-m", "first")
		run(app, "push", "-q", "-u", "origin", "HEAD:main")
		run(app, "commit", "-q", "--allow-empty", "-m", "agent's work")

		r := repos.Repo{Name: "app", URL: url, Secret: "APP_TOKEN", Push: true}
		cmd := exec.Command("sh", "-c", selfPushCommand(r))
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "APP_TOKEN=s3cret")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: push command failed: %v\n%s", url, err, out)
		}
		if got := run(base, "--git-dir", remote, "log", "-1", "--format=%s", "main"); strings.TrimSpace(got) != "agent's work" {
			t.Fatalf("%s: remote main is at %q", url, got)
		}
		if hasUnpushedCommits(context.Background(), app) {
			t.Fatalf("%s: the runner would push again", url)
		}
	}

	// The HTTPS helper hands git the URL's user and the secret's value.
	r := repos.Repo{Name: "app", URL: "https://lei@gitee.com/o/app.git", Secret: "APP_TOKEN", Push: true}
	fill := strings.Replace(selfPushCommand(r), " push origin HEAD", " credential fill", 1)
	fill = strings.Replace(fill, "git -C app ", "git ", 1)
	cmd := exec.Command("sh", "-c", fill)
	cmd.Env = append(os.Environ(), "APP_TOKEN=s3cret")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=gitee.com\n\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "username=lei") || !strings.Contains(string(out), "password=s3cret") {
		t.Fatalf("credential helper: %v\n%s", err, out)
	}

	note := runtimeInstructions(runtimeFacts{Repos: []repos.Repo{r, {Name: "docs", URL: "https://github.com/o/docs.git", Push: true}, {Name: "ro", URL: "https://x/ro.git", Secret: "T", Push: false}}})
	for _, want := range []string{"you may also push it yourself", "Never force-push", "push origin HEAD", "Otherwise, do not `git push`"} {
		if !strings.Contains(note, want) {
			t.Errorf("runtime note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "git -C ro ") || strings.Contains(note, "git -C docs ") {
		t.Errorf("a read-only or secretless repo got a push command:\n%s", note)
	}
}
