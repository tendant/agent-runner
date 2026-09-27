package thread

import (
	"context"
	"errors"
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
