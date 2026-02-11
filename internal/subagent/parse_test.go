package subagent

import (
	"testing"
)

func TestParsePlanResult_DirectJSON(t *testing.T) {
	input := `{"summary":"Fix login","steps":[{"id":"1","description":"Update auth handler","done":false}],"approach":"Modify the handler"}`
	result, err := parsePlanResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Fix login" {
		t.Errorf("expected summary 'Fix login', got '%s'", result.Summary)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
	if result.Steps[0].ID != "1" {
		t.Errorf("expected step ID '1', got '%s'", result.Steps[0].ID)
	}
	if result.Approach != "Modify the handler" {
		t.Errorf("expected approach 'Modify the handler', got '%s'", result.Approach)
	}
}

func TestParsePlanResult_EmbeddedJSON(t *testing.T) {
	input := `Here is my plan:
{"summary":"Refactor","steps":[{"id":"1","description":"Extract function","done":false},{"id":"2","description":"Add tests","done":false}],"approach":"incremental"}
Let me know if this looks good.`
	result, err := parsePlanResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Refactor" {
		t.Errorf("expected summary 'Refactor', got '%s'", result.Summary)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Steps))
	}
}

func TestParsePlanResult_InvalidInput(t *testing.T) {
	_, err := parsePlanResult("this is not json at all")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

func TestParsePlanResult_EmptySteps(t *testing.T) {
	input := `{"summary":"Nothing","steps":[],"approach":"none"}`
	_, err := parsePlanResult(input)
	if err == nil {
		t.Error("expected error for empty steps")
	}
}

func TestParsePlanResult_WithWhitespace(t *testing.T) {
	input := `
	{"summary":"Plan","steps":[{"id":"a","description":"Do it","done":false}],"approach":"direct"}
	`
	result, err := parsePlanResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Plan" {
		t.Errorf("expected summary 'Plan', got '%s'", result.Summary)
	}
}

func TestParseReviewResult_DirectJSON(t *testing.T) {
	input := `{"complete":true,"score":8,"issues":["minor style"],"suggestions":["add docs"]}`
	result, err := parseReviewResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Complete {
		t.Error("expected complete=true")
	}
	if result.Score != 8 {
		t.Errorf("expected score 8, got %d", result.Score)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(result.Issues))
	}
}

func TestParseReviewResult_EmbeddedJSON(t *testing.T) {
	input := `Review result: {"complete":false,"score":5} done.`
	result, err := parseReviewResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Complete {
		t.Error("expected complete=false")
	}
	if result.Score != 5 {
		t.Errorf("expected score 5, got %d", result.Score)
	}
}

func TestParseReviewResult_InvalidInput(t *testing.T) {
	_, err := parseReviewResult("not json")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}
