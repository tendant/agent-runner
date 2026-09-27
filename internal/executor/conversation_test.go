package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeResumingClaude puts a "claude" on PATH that appends its argv (one
// line per call) to $CAPTURE_ARGS_PATH, fails like the real CLI when asked
// to --resume the session "gone", and otherwise prints sampleStream.
func fakeResumingClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, "args.txt")
	script := `#!/bin/sh
echo "$*" >> "$CAPTURE_ARGS_PATH"
prev=""
for a in "$@"; do
  if [ "$prev" = "--resume" ] && [ "$a" = "gone" ]; then
    echo "No conversation found with session ID: gone" >&2
    exit 1
  fi
  prev="$a"
done
cat <<'EOS'
` + sampleStream + `EOS
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_ARGS_PATH", args)
	return args
}

func calls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestClaudeExecuteResuming(t *testing.T) {
	argsPath := fakeResumingClaude(t)
	e := NewClaudeExecutor("", 0)
	ws := t.TempDir()

	_, ref, err := e.ExecuteResuming(context.Background(), ws, "sys", "first", "", nil)
	if err != nil || ref == "" {
		t.Fatalf("new conversation: ref %q, err %v", ref, err)
	}
	_, ref2, err := e.ExecuteResuming(context.Background(), ws, "sys", "second", ref, nil)
	if err != nil || ref2 != ref {
		t.Fatalf("resume: ref %q (want %q), err %v", ref2, ref, err)
	}
	c := calls(t, argsPath)
	if !strings.Contains(c[0], "--session-id "+ref) || strings.Contains(c[0], "--resume") {
		t.Errorf("first call should name a new session: %s", c[0])
	}
	if !strings.Contains(c[1], "--resume "+ref) || strings.Contains(c[1], "--session-id") {
		t.Errorf("second call should resume it: %s", c[1])
	}

	_, ref3, err := e.ExecuteResuming(context.Background(), ws, "sys", "third", "gone", nil)
	if !errors.Is(err, ErrResumeFailed) || ref3 != "" {
		t.Fatalf("missing session: ref %q, err %v; want ErrResumeFailed", ref3, err)
	}
}

// fakeResumer is a ResumingExecutor that records each call's ref, system
// prompt and message, and rejects refs listed in bad.
type fakeResumer struct {
	streamingStubExec
	refs, systems, msgs []string
	bad                 map[string]bool
	n                   int
}

type streamingStubExec struct{}

func (streamingStubExec) Execute(context.Context, string, string) (*ExecutionResult, error) {
	return &ExecutionResult{}, nil
}
func (streamingStubExec) ExecuteWithSystemPrompt(context.Context, string, string, string) (*ExecutionResult, error) {
	return &ExecutionResult{}, nil
}
func (streamingStubExec) ExecuteWithLog(context.Context, string, string) (*ExecutionResult, string, error) {
	return &ExecutionResult{}, "", nil
}
func (streamingStubExec) ExecuteWithLogAndSystemPrompt(context.Context, string, string, string) (*ExecutionResult, string, error) {
	return &ExecutionResult{}, "", nil
}

func (f *fakeResumer) ResumeKind() string { return "fake" }
func (f *fakeResumer) ExecuteResuming(_ context.Context, _, sys, msg, ref string, _ func(EventKind, string)) (*ExecutionResult, string, error) {
	f.refs = append(f.refs, ref)
	f.systems = append(f.systems, sys)
	f.msgs = append(f.msgs, msg)
	if f.bad[ref] {
		return nil, "", ErrResumeFailed
	}
	if ref == "" {
		f.n++
		ref = "conv-" + string(rune('0'+f.n))
	}
	return &ExecutionResult{Output: "ok"}, ref, nil
}

func TestOneShotSession_ContinuesConversationAcrossSessions(t *testing.T) {
	fr := &fakeResumer{bad: map[string]bool{}}
	conv := &Conversation{Dir: t.TempDir()}
	b := WrapOneShot(fr)

	s1, _ := b.Start(context.Background(), t.TempDir(), SessionOptions{Conversation: conv})
	if cr, ok := s1.(ContextRetainer); !ok || !cr.RetainsContext() {
		t.Fatal("a resuming session should retain context")
	}
	s1.Prompt(context.Background(), PromptRequest{SystemPrompt: "SYS", Message: "full"})
	s1.Prompt(context.Background(), PromptRequest{Message: "incremental"})
	s1.Close()

	// A later turn: new session, same conversation.
	s2, _ := b.Start(context.Background(), t.TempDir(), SessionOptions{Conversation: conv})
	s2.Prompt(context.Background(), PromptRequest{SystemPrompt: "SYS2", Message: "turn 2"})

	if strings.Join(fr.refs, ",") != ",conv-1,conv-1" {
		t.Fatalf("refs = %q, want a new conversation then two resumes", fr.refs)
	}
	if fr.systems[1] != "SYS" {
		t.Errorf("incremental prompt system = %q, want the previous system prompt kept", fr.systems[1])
	}

	// The saved conversation disappears: start a fresh one, don't fail.
	fr.bad["conv-1"] = true
	res, err := s2.Prompt(context.Background(), PromptRequest{Message: "after loss"})
	if err != nil || res == nil {
		t.Fatalf("prompt after a lost conversation failed: %v", err)
	}
	if got := fr.refs[len(fr.refs)-2:]; got[0] != "conv-1" || got[1] != "" {
		t.Fatalf("refs after loss = %q, want a failed resume then a fresh start", got)
	}
	if conv.loadRef("fake") != "conv-2" {
		t.Fatalf("saved ref = %q, want the new conversation", conv.loadRef("fake"))
	}
}

func TestOneShotSession_NoConversationIsUnchanged(t *testing.T) {
	fr := &fakeResumer{}
	s, _ := WrapOneShot(fr).Start(context.Background(), t.TempDir(), SessionOptions{})
	if s.(ContextRetainer).RetainsContext() {
		t.Fatal("a session without a conversation must not retain context")
	}
	s.Prompt(context.Background(), PromptRequest{Message: "x"})
	if len(fr.refs) != 0 {
		t.Fatal("ExecuteResuming used without a conversation")
	}
}

func TestPiBackend_DurableSessionArgs(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "pi"), []byte(fakePiScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	argsPath := filepath.Join(tmpDir, "args.txt")
	t.Setenv("CAPTURE_ARGS_PATH", argsPath)
	t.Setenv("CAPTURE_STDIN_PATH", filepath.Join(tmpDir, "stdin.txt"))

	conv := &Conversation{Dir: filepath.Join(t.TempDir(), "backend")}
	start := func() string {
		s, err := NewPiBackend("", "").Start(context.Background(), tmpDir, SessionOptions{Conversation: conv})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), PromptRequest{Message: "hi"}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		data, _ := os.ReadFile(argsPath)
		return string(data)
	}
	first, second := start(), start()
	if strings.Contains(first, "--no-session") || !strings.Contains(first, "--session-dir\n"+filepath.Join(conv.Dir, "pi")+"\n--session-id\n") {
		t.Fatalf("args = %q, want a durable session under the conversation dir", first)
	}
	if first != second {
		t.Fatalf("second start used different args:\n%q\n%q", first, second)
	}
}
