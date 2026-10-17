package thread

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAnalysisResult_DirectJSON(t *testing.T) {
	input := `{"action":"ask","message":"What framework?"}`
	result, err := parseAnalysisResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != "ask" {
		t.Errorf("expected action 'ask', got %q", result.Action)
	}
	if result.Message != "What framework?" {
		t.Errorf("expected message 'What framework?', got %q", result.Message)
	}
}

func TestParseAnalysisResult_EmbeddedJSON(t *testing.T) {
	input := `Here is my response:
{"action":"plan","message":"I will create a Hugo site"}
Done.`
	result, err := parseAnalysisResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != "plan" {
		t.Errorf("expected action 'plan', got %q", result.Action)
	}
}

func TestParseAnalysisResult_WithWhitespace(t *testing.T) {
	input := `  {"action":"ask","message":"Which project?"}  `
	result, err := parseAnalysisResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != "ask" {
		t.Errorf("expected action 'ask', got %q", result.Action)
	}
}

func TestParseAnalysisResult_NoJSON(t *testing.T) {
	input := "This is just plain text with no JSON at all"
	_, err := parseAnalysisResult(input)
	if err == nil {
		t.Error("expected error for non-JSON input")
	}
}

func TestParseAnalysisResult_EmptyAction(t *testing.T) {
	input := `{"action":"","message":"test"}`
	_, err := parseAnalysisResult(input)
	if err == nil {
		t.Error("expected error for empty action")
	}
}

type cannedClient struct {
	out string
	err error
}

func (c cannedClient) Complete(context.Context, string) (string, error) { return c.out, c.err }

func TestClassifyTaskMessage(t *testing.T) {
	cases := []struct {
		out      string
		err      error
		fallback string
		want     string
	}{
		{`{"kind":"new"}`, nil, TaskMessageContinue, TaskMessageNew},
		{"Sure! {\"kind\": \"continue\"}", nil, TaskMessageNew, TaskMessageContinue},
		{"no json here", nil, TaskMessageNew, TaskMessageNew},
		{`{"kind":"chat"}`, nil, TaskMessageContinue, TaskMessageChat},
		{"", errors.New("down"), TaskMessageContinue, TaskMessageContinue},
	}
	for _, c := range cases {
		a := NewAnalyzer(cannedClient{c.out, c.err})
		if got := a.ClassifyTaskMessage(context.Background(), "goal", "q?", "msg", c.fallback); got != c.want {
			t.Errorf("output %q: got %q, want %q", c.out, got, c.want)
		}
	}
	var nilAnalyzer *Analyzer
	if got := nilAnalyzer.ClassifyTaskMessage(context.Background(), "", "", "", TaskMessageNew); got != TaskMessageNew {
		t.Errorf("nil analyzer: %q", got)
	}
}

func alertThread() *Thread {
	c := &Thread{ChatID: "t"}
	c.AddAlertMessage("[Automated alert] CI failed on main")
	return c
}

type recordingClient struct {
	out    string
	err    error
	prompt *string
}

func (c recordingClient) Complete(_ context.Context, p string) (string, error) {
	*c.prompt = p
	return c.out, c.err
}

// An alert is triaged, never routed like a person's request: failures tell a
// person, "execute" needs an operator policy, and "ignore" is alert-only.
func TestAnalyze_AlertTriage(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "ALERTS.md")
	cases := []struct {
		name   string
		out    string
		err    error
		policy string
		want   string
	}{
		{"llm down tells a person", "", errors.New("down"), "", "ask"},
		{"garbage tells a person", "no json", nil, "", "ask"},
		{"ignore", `{"action":"ignore","message":""}`, nil, "", "ignore"},
		{"execute without policy proposes", `{"action":"execute","message":"fixing"}`, nil, "", "plan"},
		{"execute with policy", `{"action":"execute","message":"fixing"}`, nil, "CI on main: fix it", "execute"},
		{"unknown action tells a person", `{"action":"dance","message":"x"}`, nil, "", "ask"},
	}
	for _, c := range cases {
		os.Remove(policyPath)
		if c.policy != "" {
			os.WriteFile(policyPath, []byte(c.policy), 0o600)
		}
		var prompt string
		a := NewAnalyzer(recordingClient{c.out, c.err, &prompt})
		a.SetAlertPolicyPath(policyPath)
		got, err := a.Analyze(context.Background(), alertThread())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Action != c.want {
			t.Errorf("%s: action %q, want %q", c.name, got.Action, c.want)
		}
		if got.Action == "ask" && got.Message == "" {
			t.Errorf("%s: empty message", c.name)
		}
		if c.policy != "" && !strings.Contains(prompt, c.policy) {
			t.Errorf("%s: policy missing from prompt", c.name)
		}
	}
}

// A person's message is never ignored, even if the model says so.
func TestAnalyze_PersonNeverIgnored(t *testing.T) {
	c := &Thread{ChatID: "t"}
	c.AddMessage("user", "hello")
	a := NewAnalyzer(cannedClient{out: `{"action":"ignore","message":"hi"}`})
	got, _ := a.Analyze(context.Background(), c)
	if got.Action != "ask" {
		t.Fatalf("action %q, want ask", got.Action)
	}
}
