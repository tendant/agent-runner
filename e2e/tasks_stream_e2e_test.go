package e2e

// End-to-end test of multi-turn tasks (TASKS_DESIGN.md) through a real
// agent-stream server: agent-stream (in-memory store) and agent-runner both
// run as built binaries, the test plays the human over agent-stream's HTTP
// API, and a fake `claude` CLI (testdata/mock-claude-tasks.py) stands in for
// every model call — router, classifier, planner and the agent.
//
// It needs the agent-stream server source, so it only runs when
// AGENT_STREAM_SRC points at it (the directory with cmd/server; use an
// absolute path):
//
//	AGENT_STREAM_SRC=/path/to/agent-stream/agent-stream go test ./e2e -run TasksOverAgentStream -v
//
// TestE2E_TasksRealClaude (tasks_real_claude_e2e_test.go) runs a shorter
// scenario on the same stack with the real claude CLI.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// taskStack is agent-stream plus agent-runner, running, with a human user,
// a channel and the runner's bot in it.
type taskStack struct {
	human   *client
	channel string
	dataDir string
	logs    func() string // agent-runner log tail, for failure messages
}

// startTaskStack builds and starts both servers. pathPrefix goes first on
// agent-runner's PATH (the fake claude's dir, or "" for the real CLI);
// runnerEnv adds agent-runner settings.
func startTaskStack(t *testing.T, pathPrefix string, runnerEnv ...string) *taskStack {
	t.Helper()
	src := os.Getenv("AGENT_STREAM_SRC")
	if src == "" {
		t.Skip("set AGENT_STREAM_SRC to the agent-stream server source to run")
	}
	src, _ = filepath.Abs(src)
	base := t.TempDir()

	// Build both servers.
	asBin := filepath.Join(base, "agent-stream-server")
	arBin := filepath.Join(base, "agent-runner")
	build(t, src, asBin)
	repoRoot, _ := filepath.Abs("..")
	build(t, repoRoot, arBin)

	// agent-stream, in memory.
	asURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	startProcess(t, "agent-stream", asBin, base, filepath.Join(base, "agent-stream.log"),
		cleanEnv("PORT="+strings.TrimPrefix(asURL, "http://127.0.0.1:"), "UPLOADS_DIR="+filepath.Join(base, "uploads")))
	waitHTTP(t, asURL+"/v2/channels")

	// The human, a channel, and the bot as a member.
	human := &client{t: t, base: asURL}
	var sess struct {
		AccessToken string `json:"access_token"`
	}
	human.do("POST", "/v2/auth/tokens", map[string]any{"device_id": "e2e-human"}, &sess)
	human.token = sess.AccessToken
	var ch struct {
		ID string `json:"channel_id"`
	}
	human.do("POST", "/v2/channels", map[string]any{"title": "tasks e2e"}, &ch)
	var bot struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
	}
	human.do("POST", "/v2/bots", map[string]any{"name": "runner"}, &bot)
	human.do("POST", "/v2/channels/"+ch.ID+"/members", map[string]any{"user_id": bot.UserID}, nil)

	// agent-runner with tasks on.
	path := os.Getenv("PATH")
	if pathPrefix != "" {
		path = pathPrefix + string(os.PathListSeparator) + path
	}
	dataDir := filepath.Join(base, "runner")
	runnerURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	runDir := filepath.Join(base, "cwd") // no .env here
	os.MkdirAll(runDir, 0o755)
	env := append([]string{
		"PATH=" + path,
		"DATA_DIR=" + dataDir,
		"API_BIND=" + strings.TrimPrefix(runnerURL, "http://"),
		"AGENT_CLI=claude",
		"AGENT_TASKS_ENABLED=true",
		"AGENT_REVIEWER_ENABLED=false",
		"WELCOME_ENABLED=false",
		"STREAM_SERVER_URL=" + asURL,
		"STREAM_BOT_TOKEN=" + bot.Token,
		"STREAM_CHANNEL_IDS=" + ch.ID,
	}, runnerEnv...)
	logPath := filepath.Join(base, "agent-runner.log")
	startProcess(t, "agent-runner", arBin, runDir, logPath, cleanEnv(env...))
	waitHTTP(t, runnerURL+"/health")
	logs := func() string {
		a, _ := os.ReadFile(logPath)
		return tail(string(a), 60)
	}
	// A bot's first connection skips the channel's existing history, so the
	// human only speaks once the bot is listening live.
	waitFor(t, 30*time.Second, "the stream bot to connect", logs, func() bool {
		return strings.Contains(logs(), "stream bot: SSE connected")
	})
	return &taskStack{human: human, channel: ch.ID, dataDir: dataDir, logs: logs}
}

// newThread posts a top-level message and returns its thread ID.
func (s *taskStack) newThread(content string) string {
	var root struct {
		MessageID string `json:"message_id"`
	}
	s.human.do("POST", "/v2/channels/"+s.channel+"/messages", map[string]any{"content": content}, &root)
	return root.MessageID
}

// reply posts a message in a thread.
func (s *taskStack) reply(thread, content string) {
	s.human.do("POST", "/v2/threads/"+thread+"/messages", map[string]any{"content": content}, nil)
}

func TestE2E_TasksOverAgentStream(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for the fake claude CLI")
	}
	mockBin := t.TempDir()
	mock, _ := filepath.Abs("testdata/mock-claude-tasks.py")
	if err := os.Symlink(mock, filepath.Join(mockBin, "claude")); err != nil {
		t.Fatal(err)
	}
	claudeLog := filepath.Join(t.TempDir(), "claude-calls.jsonl")
	st := startTaskStack(t, mockBin, "AGENT_MAX_ITERATIONS=3", "MOCK_CLAUDE_LOG="+claudeLog)
	human, dataDir, logs := st.human, st.dataDir, st.logs

	// 1. A new thread: the agent works, then asks a question.
	thread := st.newThread("deploy the app")
	waitBotMessage(t, human, thread, "Which environment should I deploy to?", logs)
	rec := readTask(t, dataDir, thread)
	if rec.Status != "awaiting_input" || len(rec.Turns) != 1 {
		t.Fatalf("after the question: status %s, %d turns", rec.Status, len(rec.Turns))
	}
	workspace := rec.Workspace
	if _, err := os.Stat(filepath.Join(workspace, "workspace", "notes.txt")); err != nil {
		t.Fatalf("turn 1's scratch work is not in the task workspace: %v", err)
	}

	// 2. The answer resumes the same task in the same workspace.
	human.do("POST", "/v2/threads/"+thread+"/messages", map[string]any{"content": "staging"}, nil)
	waitBotMessage(t, human, thread, "Task done", logs)
	rec = readTask(t, dataDir, thread)
	if rec.Status != "done" || len(rec.Turns) != 2 || rec.Workspace != workspace {
		t.Fatalf("after the answer: status %s, %d turns, workspace %s (was %s)", rec.Status, len(rec.Turns), rec.Workspace, workspace)
	}
	deploy, _ := os.ReadFile(filepath.Join(workspace, "workspace", "deploy.txt"))
	if !strings.Contains(string(deploy), "staging (scratch kept)") {
		t.Fatalf("turn 2 did not continue turn 1's workspace: deploy.txt = %q", deploy)
	}
	if len(rec.Plan) != 2 || !rec.Plan[0].Done || !rec.Plan[1].Done {
		t.Fatalf("plan not tracked across turns: %+v", rec.Plan)
	}
	done := botMessageContaining(t, human, thread, "Task done")
	if !strings.Contains(done, "✅ 1. prepare the deploy") {
		t.Fatalf("done notice has no checklist: %q", done)
	}
	checkClaudeResumed(t, claudeLog, filepath.Join(workspace, "workspace"))

	// 3. Small talk gets a reply and starts no turn.
	human.do("POST", "/v2/threads/"+thread+"/messages", map[string]any{"content": "thanks!"}, nil)
	waitBotMessage(t, human, thread, "You're welcome!", logs)
	if rec = readTask(t, dataDir, thread); len(rec.Turns) != 2 {
		t.Fatalf("small talk started a turn (%d turns)", len(rec.Turns))
	}

	// 4. Feedback on the done task revises its plan and continues it.
	human.do("POST", "/v2/threads/"+thread+"/messages", map[string]any{"content": "please also deploy to prod"}, nil)
	waitBotMessageCount(t, human, thread, "Task done", 2, logs)
	rec = readTask(t, dataDir, thread)
	if rec.Status != "done" || len(rec.Turns) != 3 || rec.Workspace != workspace {
		t.Fatalf("after feedback: status %s, %d turns", rec.Status, len(rec.Turns))
	}
	if len(rec.Plan) != 3 || rec.Plan[2].Text != "also deploy to prod" || !rec.Plan[2].Done {
		t.Fatalf("plan not revised for the feedback: %+v", rec.Plan)
	}

	// 5. /cancel on a second task waiting for an answer releases its workspace.
	thread2 := st.newThread("deploy the docs site")
	waitBotMessage(t, human, thread2, "Which environment should I deploy to?", logs)
	ws2 := readTask(t, dataDir, thread2).Workspace
	if ws2 == workspace {
		t.Fatal("a second thread shared the first thread's task workspace")
	}
	human.do("POST", "/v2/threads/"+thread2+"/messages", map[string]any{"content": "/cancel"}, nil)
	waitFor(t, 30*time.Second, "cancelled task", logs, func() bool {
		return readTask(t, dataDir, thread2).Status == "cancelled"
	})
	waitFor(t, 30*time.Second, "cancelled task's workspace removed", logs, func() bool {
		_, err := os.Stat(ws2)
		return os.IsNotExist(err)
	})
	// The first thread's finished task is untouched.
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("cancelling one thread's task removed another's workspace: %v", err)
	}
}

// taskRecord mirrors the fields of task.Record the test checks.
type taskRecord struct {
	Status    string `json:"status"`
	Workspace string `json:"workspace"`
	Turns     []struct {
		SessionID string `json:"session_id"`
	} `json:"turns"`
	Plan []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
		Done bool   `json:"done"`
	} `json:"plan"`
}

func readTask(t *testing.T, dataDir, thread string) taskRecord {
	t.Helper()
	var rec taskRecord
	data, err := os.ReadFile(filepath.Join(dataDir, "state", "tasks", thread+".json"))
	if err != nil {
		return rec
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("task record: %v", err)
	}
	return rec
}

// checkClaudeResumed asserts that the agent calls made in the task
// workspace continue one Claude conversation: the first names a new session,
// every later one resumes it.
func checkClaudeResumed(t *testing.T, logPath, checkout string) {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if real, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = real // macOS: /var → /private/var, as the CLI's cwd reports it
	}
	var sessionID string
	agentCalls := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var call struct {
			Cwd  string   `json:"cwd"`
			Args []string `json:"args"`
		}
		if json.Unmarshal(sc.Bytes(), &call) != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(call.Cwd); err == nil {
			call.Cwd = real
		}
		if call.Cwd != checkout {
			continue // router/planner calls run elsewhere
		}
		agentCalls++
		flag, val := sessionFlag(call.Args)
		if agentCalls == 1 {
			if flag != "--session-id" {
				t.Fatalf("first agent call should start a named session, args %v", call.Args)
			}
			sessionID = val
			continue
		}
		if flag != "--resume" || val != sessionID {
			t.Fatalf("agent call %d should resume %s, got %s %s", agentCalls, sessionID, flag, val)
		}
	}
	if agentCalls < 2 {
		t.Fatalf("saw %d agent calls in the task workspace, want at least 2", agentCalls)
	}
}

func sessionFlag(args []string) (flag, val string) {
	for i, a := range args[:len(args)-1] {
		if a == "--session-id" || a == "--resume" {
			return a, args[i+1]
		}
	}
	return "", ""
}

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body, out any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode %v: %s", method, path, err, data)
		}
	}
}

type streamMessage struct {
	SenderKind string `json:"sender_kind"`
	Content    string `json:"content"`
}

func (c *client) botMessages(thread string) []string {
	c.t.Helper()
	var msgs []streamMessage
	c.do("GET", "/v2/threads/"+thread+"/messages", nil, &msgs)
	var out []string
	for _, m := range msgs {
		if m.SenderKind == "bot" {
			out = append(out, m.Content)
		}
	}
	return out
}

func countContaining(msgs []string, want string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, want) {
			n++
		}
	}
	return n
}

func botMessageContaining(t *testing.T, c *client, thread, want string) string {
	t.Helper()
	for _, m := range c.botMessages(thread) {
		if strings.Contains(m, want) {
			return m
		}
	}
	t.Fatalf("no bot message containing %q", want)
	return ""
}

func waitBotMessage(t *testing.T, c *client, thread, want string, logs func() string) {
	t.Helper()
	waitBotMessageCount(t, c, thread, want, 1, logs)
}

func waitBotMessageCount(t *testing.T, c *client, thread, want string, n int, logs func() string) {
	t.Helper()
	waitFor(t, 90*time.Second, fmt.Sprintf("%d bot message(s) containing %q", n, want), func() string {
		return logs() + "\n--- bot messages in thread ---\n" + strings.Join(c.botMessages(thread), "\n---\n")
	}, func() bool {
		return countContaining(c.botMessages(thread), want) >= n
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, diag func() string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\n%s", what, diag())
}

func build(t *testing.T, dir, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, "./cmd/server")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", dir, err, b)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// cleanEnv is the test's environment without model credentials or database
// settings (so both servers use the fakes and in-memory stores), plus extra.
func cleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv, '=')]
		if strings.HasSuffix(k, "_API_KEY") || k == "DATABASE_URL" || k == "PATH" ||
			strings.HasPrefix(k, "AGENT_") || strings.HasPrefix(k, "STREAM_") || k == "DATA_DIR" ||
			k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") { // don't look nested in a Claude Code session
			continue
		}
		env = append(env, kv)
	}
	hasPath := false
	for _, kv := range extra {
		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
	}
	if !hasPath {
		env = append(env, "PATH="+os.Getenv("PATH"))
	}
	return append(env, extra...)
}

func startProcess(t *testing.T, name, bin, dir, logPath string, env []string) {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			// Drop the test's own polling of thread messages.
			var kept []string
			for _, l := range strings.Split(string(data), "\n") {
				if !strings.Contains(l, "method=GET path=/v2/threads/") {
					kept = append(kept, l)
				}
			}
			t.Logf("--- %s log (tail) ---\n%s", name, tail(strings.Join(kept, "\n"), 80))
		}
	})
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never came up", url)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
