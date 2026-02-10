package conversation

import (
	"testing"
)

func TestParseAnalysisResult_DirectJSON(t *testing.T) {
	input := `{"action":"ask","message":"What framework?","project":""}`
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
{"action":"plan","message":"I will create a Hugo site","project":"my-site"}
Done.`
	result, err := parseAnalysisResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != "plan" {
		t.Errorf("expected action 'plan', got %q", result.Action)
	}
	if result.Project != "my-site" {
		t.Errorf("expected project 'my-site', got %q", result.Project)
	}
}

func TestParseAnalysisResult_WithWhitespace(t *testing.T) {
	input := `  {"action":"ask","message":"Which project?","project":""}  `
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
	input := `{"action":"","message":"test","project":""}`
	_, err := parseAnalysisResult(input)
	if err == nil {
		t.Error("expected error for empty action")
	}
}
