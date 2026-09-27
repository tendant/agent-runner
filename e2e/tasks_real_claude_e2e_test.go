package e2e

// TestE2E_TasksRealClaude runs a multi-turn task through the same stack as
// TestE2E_TasksOverAgentStream, but with the real claude CLI for every model
// call (router, classifier, planner and agent) — it checks that a real model
// follows the task protocol: asks through _progress.json, continues in the
// same workspace and conversation, and acts on feedback. It makes billed
// model calls with the local claude login, so it only runs when asked:
//
//	AGENT_E2E_REAL_CLAUDE=1 AGENT_STREAM_SRC=/path/to/agent-stream/agent-stream \
//	  go test ./e2e -run TasksRealClaude -v -timeout 45m
//
// The model's wording varies, so it checks outcomes (files, task records),
// not text.

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const realTurnTimeout = 12 * time.Minute

func TestE2E_TasksRealClaude(t *testing.T) {
	if os.Getenv("AGENT_E2E_REAL_CLAUDE") == "" {
		t.Skip("set AGENT_E2E_REAL_CLAUDE=1 to run against the real claude CLI (billed)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH")
	}
	model := os.Getenv("AGENT_E2E_MODEL")
	if model == "" {
		model = "sonnet"
	}
	st := startTaskStack(t, "",
		"AGENT_MODEL="+model,
		"AGENT_MAX_ITERATIONS=4",
		"AGENT_MAX_TOTAL_SECONDS=900",
		"AGENT_MAX_ITERATION_SECONDS=300",
	)
	human := st.human
	transcript := func(thread string) string {
		return strings.Join(human.botMessages(thread), "\n----\n")
	}
	var threadID string
	defer func() {
		t.Logf("--- conversation ---\n%s", transcript(threadID))
	}()

	// 1. A task the agent cannot finish without asking.
	// The router answers up-front clarifications itself, so the question has
	// to arise in the middle of the agent's work.
	threadID = st.newThread("In your working directory, create two files: staging.env containing the line ENV=staging, and production.env containing the line ENV=production. After creating them, ask me which of the two is the deploy target and wait for my answer; then create deploy-target.txt containing just that environment's name.")
	waitTaskStarted(t, st, threadID)
	waitTurnEnd(t, st, threadID, 1)
	rec := readTask(t, st.dataDir, threadID)
	if rec.Status != "awaiting_input" {
		t.Fatalf("the agent did not stop to ask (task status %q); bot said:\n%s", rec.Status, transcript(threadID))
	}
	workspace := rec.Workspace
	if f := findFile(workspace, "deploy-target.txt"); f != "" {
		t.Fatalf("the agent created %s before getting the answer", f)
	}
	if findFile(workspace, "staging.env") == "" {
		t.Fatal("the first turn's work (staging.env) is missing")
	}

	// 2. The answer: the same task continues and finishes.
	st.reply(threadID, "staging")
	waitTurnEnd(t, st, threadID, 2)
	rec = readTask(t, st.dataDir, threadID)
	if rec.Workspace != workspace || len(rec.Turns) != 2 {
		t.Fatalf("the answer did not continue the task: workspace %s (was %s), %d turns", rec.Workspace, workspace, len(rec.Turns))
	}
	if rec.Status != "done" {
		t.Fatalf("task status after the answer = %q, want done", rec.Status)
	}
	target := findFile(workspace, "deploy-target.txt")
	if target == "" {
		t.Fatal("deploy-target.txt was not created after the answer")
	}
	if data, _ := os.ReadFile(target); !strings.Contains(strings.ToLower(string(data)), "staging") {
		t.Fatalf("deploy-target.txt = %q, want staging", data)
	}
	if _, err := os.Stat(filepath.Join(workspace, "state", "backend", "claude-session")); err != nil {
		t.Fatalf("no saved claude conversation for the task: %v", err)
	}

	// 3. Feedback on the finished task continues it in the same workspace.
	st.reply(threadID, "Also create a file named reason.txt with one sentence explaining why staging is a safe first target.")
	waitTurnEnd(t, st, threadID, 3)
	if rec = readTask(t, st.dataDir, threadID); rec.Status == "awaiting_input" && strings.Contains(transcript(threadID), "Proceed with this plan?") {
		st.reply(threadID, "yes")
		waitTurnEnd(t, st, threadID, 4)
		rec = readTask(t, st.dataDir, threadID)
	}
	if rec.Workspace != workspace || rec.Status != "done" {
		t.Fatalf("after feedback: status %q, workspace %s (was %s)", rec.Status, rec.Workspace, workspace)
	}
	if findFile(workspace, "reason.txt") == "" {
		t.Fatal("reason.txt was not created for the feedback")
	}
	if findFile(workspace, "deploy-target.txt") == "" {
		t.Fatal("the feedback turn lost the earlier turn's file")
	}
}

// waitTaskStarted fails fast when the router answers without starting a task.
func waitTaskStarted(t *testing.T, st *taskStack, thread string) {
	t.Helper()
	waitFor(t, 3*time.Minute, "a task to start (the router may have replied itself)", func() string {
		return st.logs() + "\n--- bot messages ---\n" + strings.Join(st.human.botMessages(thread), "\n----\n")
	}, func() bool {
		return len(readTask(t, st.dataDir, thread).Turns) > 0
	})
}

// waitTurnEnd waits until the thread's task has at least n turns and the
// last one has ended (the task is no longer working).
func waitTurnEnd(t *testing.T, st *taskStack, thread string, n int) {
	t.Helper()
	waitFor(t, realTurnTimeout, "turn to end", func() string {
		return st.logs() + "\n--- bot messages ---\n" + strings.Join(st.human.botMessages(thread), "\n----\n")
	}, func() bool {
		rec := readTask(t, st.dataDir, thread)
		return len(rec.Turns) >= n && rec.Status != "" && rec.Status != "working"
	})
	// The notice is posted right after the record is updated.
	time.Sleep(2 * time.Second)
}

// findFile returns the path of the first file named name under root.
func findFile(root, name string) string {
	var found string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = p
		}
		return nil
	})
	return found
}
