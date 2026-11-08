package e2e

// TestE2E_ProactiveRealClaude runs the proactive agent (PROACTIVE_DESIGN.md)
// end to end on a sample task: a small Go repo, sample-app, whose tests fail.
// agent-stream and agent-runner run as built binaries (taskStack), the test
// plays the operator over agent-stream's HTTP API, and every model call is
// the real claude CLI, sandboxed (AGENT_SANDBOX=strict).
//
//  1. Propose: a due agenda item is triaged; the ci class is at propose, so a
//     plan is posted. A yes runs it, and the fix lands: tests pass, the test
//     file is untouched.
//  2. Run unasked: /trust test-fix notify, then a commit breaks the tests
//     again. The repo-commits signal raises it, the run starts by itself in
//     a thread of its own, and its fix lands too.
//  3. Demotion: the run's commit is reverted on the remote, and the next
//     check drops test-fix back to propose, saying why.
//  4. Proposal and eval: asked in chat, the agent proposes a change to
//     prompt.md; /proposal eval replays an eval of the same task with and
//     without it. A comparison is posted, and nothing of the replays reaches
//     the remote.
//
// It makes billed model calls (a few dollars), so it only runs when asked:
//
//	AGENT_E2E_REAL_CLAUDE=1 AGENT_STREAM_SRC=/path/to/agent-stream/agent-stream \
//	CLAUDE_CODE_OAUTH_TOKEN=... go test ./e2e -run ProactiveRealClaude -v -timeout 75m
//
// A sandboxed claude needs CLAUDE_CODE_OAUTH_TOKEN (`claude setup-token`);
// the claude install and the Go toolchain are added to the sandbox's read
// scope. The model's wording varies: the test checks outcomes (the remote's
// tests, trust levels, the runner's records), not text.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const proactiveStageTimeout = 15 * time.Minute

// The sample task: Sum subtracts. Max is correct until stage 2 breaks it.
var sampleApp = map[string]string{
	"go.mod": "module sample\n\ngo 1.21\n",
	"calc.go": `package sample

// Sum adds two numbers.
func Sum(a, b int) int { return a - b }

// Max is the larger of two numbers.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`,
	"calc_test.go": `package sample

import "testing"

func TestSum(t *testing.T) {
	if got := Sum(2, 3); got != 5 {
		t.Fatalf("Sum(2, 3) = %d, want 5", got)
	}
}

func TestMax(t *testing.T) {
	if got := Max(2, 7); got != 7 {
		t.Fatalf("Max(2, 7) = %d, want 7", got)
	}
}
`,
}

const brokenMax = `package sample

// Sum adds two numbers.
func Sum(a, b int) int { return a + b }

// Max is the larger of two numbers.
func Max(a, b int) int {
	if a < b {
		return a
	}
	return b
}
`

const sampleProactivePolicy = `---
signals: repo-commits
---
Failing tests in sample-app: fix them, as the test-fix class. Anything else: a short note at most.

# Action classes

- test-fix: go test fails in sample-app. Find the bug in the code under test, fix it (never edit the tests), run go test ./..., commit and push.
  trust: propose
`

func TestE2E_ProactiveRealClaude(t *testing.T) {
	if os.Getenv("AGENT_E2E_REAL_CLAUDE") == "" {
		t.Skip("set AGENT_E2E_REAL_CLAUDE=1 to run against the real claude CLI (billed)")
	}
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if token == "" {
		t.Skip("set CLAUDE_CODE_OAUTH_TOKEN (claude setup-token): the sandboxed claude CLI needs it")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is needed in the sandbox for the sample task")
	}

	// The sandbox reads only system paths and the run's dirs: add the claude
	// install (the CLI is a link into its versions dir) and Go.
	readAllow := []string{filepath.Dir(claude)}
	if real, err := filepath.EvalSymlinks(claude); err == nil {
		readAllow = append(readAllow, filepath.Dir(real))
	}
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		readAllow = append(readAllow, strings.TrimSpace(string(out)))
	}

	remote, seed := newSampleRemote(t)
	st := startTaskStack(t, "",
		"AGENT_CLI=claude",
		"AGENT_MODEL=sonnet",
		"AGENT_MAX_ITERATIONS=4",
		"AGENT_MAX_TOTAL_SECONDS=900",
		"AGENT_MAX_ITERATION_SECONDS=300",
		"AGENT_SANDBOX=strict",
		"AGENT_SANDBOX_READ_ALLOW="+strings.Join(readAllow, ","),
		"CLAUDE_CODE_OAUTH_TOKEN="+token,
		// The checks stay off: every check here is a forced /proactive tick,
		// so none starts on its own between the stages.
		"AGENT_PROACTIVE=off",
		"AGENT_PROACTIVE_BUDGET_USD=20",
	)
	h := st.human
	defer func() {
		if t.Failed() {
			t.Logf("--- channel ---\n%s", channelTranscript(st))
		}
	}()

	// Setup, as an operator would: where proactive messages go, the repo,
	// the goals and the policy.
	command(t, st, "/config set AGENT_PROACTIVE_CHAT "+st.channel, "AGENT_PROACTIVE_CHAT")
	mem := filepath.Join(st.dataDir, "memory")
	os.MkdirAll(mem, 0o755)
	writeFile(t, filepath.Join(st.dataDir, "repos.json"), `[{"name":"sample-app","url":"`+remote+`","push":true}]`)
	writeFile(t, filepath.Join(mem, "GOALS.md"), "Keep go test ./... green on sample-app's main branch.\n")
	writeFile(t, filepath.Join(mem, "PROACTIVE.md"), sampleProactivePolicy)
	writeFile(t, filepath.Join(mem, "agenda.md"), "- [ ] go test fails on sample-app main: fix it · due: 2026-01-01 · id: sample-red\n")
	testFile := sampleApp["calc_test.go"]

	// 1. Propose: the class is at propose, so the fix waits for a yes.
	t.Log("stage 1: a plan, and a yes")
	command(t, st, "/proactive tick", "Proactive check")
	plan := waitRoot(t, st, "Proceed? (yes/no)", nil, 5*time.Minute)
	st.reply(plan, "yes")
	waitRemote(t, st, remote, "the plan's fix to land", func(r remoteState) bool { return r.pass })
	if r := readRemote(t, remote); r.testFile != testFile {
		t.Fatalf("the fix edited calc_test.go:\n%s", r.testFile)
	}

	// 2. Run unasked: raise test-fix and break the tests again. A new commit
	// alone says nothing of the tests (the repo-commits signal reports it as
	// context), so the breakage is raised the way a known problem is: a due
	// agenda item. The run starts by itself.
	t.Log("stage 2: a run nobody asked for")
	command(t, st, "/trust test-fix notify", "propose → notify")
	pushSample(t, seed, map[string]string{"calc.go": brokenMax}, "refactor: tidy Max")
	if r := readRemote(t, remote); r.pass {
		t.Fatal("the breaking commit did not break the tests")
	}
	writeFile(t, filepath.Join(mem, "agenda.md"), "- [x] go test fails on sample-app main: fix it · id: sample-red\n"+
		"- [ ] go test fails again on sample-app main since the commit \"refactor: tidy Max\" (TestMax): fix it · due: 2026-01-01 · id: sample-red-max\n")
	seen := rootIDs(t, st)
	command(t, st, "/proactive tick", "Proactive check")
	run := waitRoot(t, st, "**Proactive run** (test-fix", seen, 5*time.Minute)
	waitRemote(t, st, remote, "the run's fix to land", func(r remoteState) bool { return r.pass })
	if r := readRemote(t, remote); r.testFile != testFile {
		t.Fatalf("the run edited calc_test.go:\n%s", r.testFile)
	}
	var done proactiveRun
	waitFor(t, 5*time.Minute, "the run to be recorded with its commits", st.logs, func() bool {
		for _, r := range readProactiveState(t, st.dataDir).Done {
			if r.Thread == run && len(r.Commits) > 0 {
				done = r
				return true
			}
		}
		return false
	})
	if done.Result != "succeeded" {
		t.Fatalf("the run is recorded as %q", done.Result)
	}
	if c := readTrust(t, st.dataDir)["test-fix"]; c.Level != "notify" || c.History.Executed != 1 {
		t.Fatalf("test-fix after the run: %+v", c)
	}

	// 3. Demotion: revert the run's commit; the next check demotes test-fix.
	t.Log("stage 3: a revert of the run's work")
	gitIn(t, seed, "pull", "-q", "origin", "main")
	gitIn(t, seed, "revert", "--no-edit", done.Commits[0].SHA)
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	seen = rootIDs(t, st)
	command(t, st, "/proactive tick", "Proactive check")
	waitFor(t, 5*time.Minute, "test-fix to be demoted", st.logs, func() bool {
		return readTrust(t, st.dataDir)["test-fix"].Level == "propose"
	})
	notice := waitRoot(t, st, "was reverted", seen, time.Minute)
	if c := readTrust(t, st.dataDir)["test-fix"]; c.History.Demoted != 1 || !strings.Contains(c.Reason, done.Commits[0].SHA[:12]) {
		t.Fatalf("test-fix after the revert: %+v (notice in %s)", c, notice)
	}

	// 4. A proposal, compared on an eval of the same task.
	t.Log("stage 4: a proposal and its eval")
	seen = rootIDs(t, st)
	ask := st.newThread("Do not change any code or repos. Write one proposal in _proposals/, as your instructions describe, to change prompt.md so that it says: when fixing a failing test, fix the code under test and never edit the tests. Use the sessions of this chat as evidence.")
	var proposal, proposalID string
	waitFor(t, proactiveStageTimeout, "a proposal to be posted", func() string { return transcript(h, ask) }, func() bool {
		if msgs := h.botMessages(ask); len(msgs) > 0 && strings.Contains(msgs[len(msgs)-1], "Proceed?") {
			st.reply(ask, "yes") // the router planned it first
			time.Sleep(5 * time.Second)
		}
		id, ok := findRoot(t, st, "**Proposal p-", seen)
		proposal = id
		return ok
	})
	m := regexp.MustCompile(`Proposal (p-[0-9a-f]+)`).FindStringSubmatch(strings.Join(h.botMessages(proposal), "\n"))
	if m == nil {
		t.Fatal("no proposal id in the proposal's thread")
	}
	proposalID = m[1]
	command(t, st, "/evals set sample-fix ---\nfile: sample-app/go.mod\ncommand: cd sample-app && go test ./...\n---\nIn sample-app, go test ./... fails. Fix the code (not the tests) so that it passes, and commit.", "ok saved eval sample-fix")
	before := readRemote(t, remote).head
	command(t, st, "/proposal eval "+proposalID, "Replaying 1 eval(s)")
	waitFor(t, 2*proactiveStageTimeout, "the comparison", func() string { return transcript(h, proposal) }, func() bool {
		return countContaining(h.botMessages(proposal), "**Eval of proposal "+proposalID) > 0
	})
	report := botMessageContaining(t, h, proposal, "**Eval of proposal "+proposalID)
	t.Logf("comparison:\n%s", report)
	if !strings.Contains(report, "2 replays") || !strings.Contains(report, "| sample-fix |") {
		t.Fatalf("the comparison:\n%s", report)
	}
	if after := readRemote(t, remote).head; after != before {
		t.Fatalf("a replay pushed: the remote moved from %s to %s", before, after)
	}
}

// newSampleRemote makes a bare remote with sampleApp on main, and a clone
// of it for the test to push from.
func newSampleRemote(t *testing.T) (remote, seed string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "sample-app.git")
	seed = filepath.Join(base, "seed")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", remote)
	gitIn(t, base, "clone", "-q", remote, seed)
	pushSample(t, seed, sampleApp, "initial")
	return remote, seed
}

func pushSample(t *testing.T, seed string, files map[string]string, message string) {
	t.Helper()
	for name, content := range files {
		writeFile(t, filepath.Join(seed, name), content)
	}
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", message)
	gitIn(t, seed, "pull", "-q", "--rebase", "origin", "main")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=operator", "GIT_AUTHOR_EMAIL=op@e2e.invalid",
		"GIT_COMMITTER_NAME=operator", "GIT_COMMITTER_EMAIL=op@e2e.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil && !(len(args) > 0 && args[0] == "pull" && strings.Contains(string(out), "couldn't find remote ref")) {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// remoteState is main on the remote: its head, whether its tests pass, and
// its test file.
type remoteState struct {
	head     string
	pass     bool
	testFile string
	output   string
}

func readRemote(t *testing.T, remote string) remoteState {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "check")
	if out, err := exec.Command("git", "clone", "-q", "-b", "main", remote, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone the remote: %v\n%s", err, out)
	}
	var r remoteState
	r.head = gitIn(t, dir, "rev-parse", "HEAD")
	data, _ := os.ReadFile(filepath.Join(dir, "calc_test.go"))
	r.testFile = string(data)
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-count=1")
	out, err := cmd.CombinedOutput()
	r.pass, r.output = err == nil, string(out)
	return r
}

// waitRemote polls the remote until cond holds.
func waitRemote(t *testing.T, st *taskStack, remote, what string, cond func(remoteState) bool) {
	t.Helper()
	deadline := time.Now().Add(proactiveStageTimeout)
	for {
		r := readRemote(t, remote)
		if cond(r) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; go test on the remote:\n%s\n%s", what, r.output, st.logs())
		}
		time.Sleep(20 * time.Second)
	}
}

// command sends a chat command as a new thread and waits for its reply.
func command(t *testing.T, st *taskStack, text, want string) {
	t.Helper()
	thread := st.newThread(text)
	waitFor(t, 3*time.Minute, "the reply to "+strings.SplitN(text, "\n", 2)[0], func() string {
		return st.logs() + "\n--- bot messages ---\n" + transcript(st.human, thread)
	}, func() bool {
		msgs := st.human.botMessages(thread)
		for _, m := range msgs {
			if strings.HasPrefix(m, "error:") {
				t.Fatalf("%s: %s", strings.SplitN(text, "\n", 2)[0], m)
			}
		}
		return countContaining(msgs, want) > 0
	})
}

func transcript(c *client, thread string) string {
	return strings.Join(c.botMessages(thread), "\n----\n")
}

type rootMessage struct {
	MessageID  string `json:"message_id"`
	SenderKind string `json:"sender_kind"`
	Content    string `json:"content"`
}

func roots(t *testing.T, st *taskStack) []rootMessage {
	t.Helper()
	var page struct {
		Data []rootMessage `json:"data"`
	}
	st.human.do("GET", "/v2/channels/"+st.channel+"/messages?limit=200", nil, &page)
	return page.Data
}

func rootIDs(t *testing.T, st *taskStack) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, m := range roots(t, st) {
		ids[m.MessageID] = true
	}
	return ids
}

// findRoot is a thread the bot started, not in seen, whose first message
// contains want.
func findRoot(t *testing.T, st *taskStack, want string, seen map[string]bool) (string, bool) {
	t.Helper()
	for _, m := range roots(t, st) {
		if m.SenderKind == "bot" && !seen[m.MessageID] && strings.Contains(m.Content, want) {
			return m.MessageID, true
		}
	}
	return "", false
}

func waitRoot(t *testing.T, st *taskStack, want string, seen map[string]bool, timeout time.Duration) string {
	t.Helper()
	var id string
	waitFor(t, timeout, "a bot thread containing "+want, func() string { return st.logs() + "\n--- channel ---\n" + channelTranscript(st) }, func() bool {
		var ok bool
		id, ok = findRoot(t, st, want, seen)
		return ok
	})
	return id
}

func channelTranscript(st *taskStack) string {
	var b strings.Builder
	var page struct {
		Data []rootMessage `json:"data"`
	}
	st.human.do("GET", "/v2/channels/"+st.channel+"/messages?limit=200", nil, &page)
	for _, m := range page.Data {
		b.WriteString("=== " + m.SenderKind + ": " + firstLine(m.Content) + "\n")
		for _, r := range st.human.botMessages(m.MessageID) {
			b.WriteString("  - " + firstLine(r) + "\n")
		}
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// The runner's own records, read from its data dir.

type proactiveRun struct {
	Class   string `json:"class"`
	Result  string `json:"result"`
	Thread  string `json:"thread"`
	Commits []struct {
		Repo, SHA, Subject string
	} `json:"commits"`
}

type proactiveState struct {
	Done map[string]proactiveRun `json:"done"`
}

func readProactiveState(t *testing.T, dataDir string) proactiveState {
	t.Helper()
	var st proactiveState
	data, err := os.ReadFile(filepath.Join(dataDir, "state", "proactive", "state.json"))
	if err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

type trustClass struct {
	Level   string `json:"level"`
	Reason  string `json:"reason"`
	History struct {
		Executed, Succeeded, Failed, Demoted int
	} `json:"history"`
}

func readTrust(t *testing.T, dataDir string) map[string]trustClass {
	t.Helper()
	m := map[string]trustClass{}
	data, err := os.ReadFile(filepath.Join(dataDir, "state", "trust.json"))
	if err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}
