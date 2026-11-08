package thread

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTriageProactive(t *testing.T) {
	cases := []struct {
		name   string
		client cannedClient
		want   string
	}{
		{"plan", cannedClient{out: `{"action":"plan","message":"I would rerun the job"}`}, "plan"},
		{"execute is advice for the runner", cannedClient{out: `{"action":"execute","class":"ci-fix","message":"rerunning"}`}, "execute"},
		{"ignore", cannedClient{out: `{"action":"ignore","message":""}`}, "ignore"},
		{"unknown action is a note", cannedClient{out: `{"action":"dance","message":"hm"}`}, "ask"},
		{"model error falls back to a note", cannedClient{err: errors.New("down")}, "ask"},
		{"prose falls back to a note", cannedClient{out: "sure, sounds good"}, "ask"},
	}
	for _, c := range cases {
		res, err := NewAnalyzer(c.client).TriageProactive(context.Background(), "## Due agenda items\n- x", "")
		if err != nil || res.Action != c.want {
			t.Errorf("%s: %+v %v, want %s", c.name, res, err, c.want)
		}
		if res.Action != "ignore" && res.Message == "" {
			t.Errorf("%s: no message", c.name)
		}
	}
	if res, _ := NewAnalyzer(cannedClient{out: `{"action":"execute","class":"ci-fix","message":"m"}`}).TriageProactive(context.Background(), "r", ""); res.Class != "ci-fix" {
		t.Errorf("class %q", res.Class)
	}
	res, _ := NewAnalyzer(cannedClient{err: errors.New("down")}).TriageProactive(context.Background(), "REPORT-TEXT", "")
	if !strings.Contains(res.Message, "REPORT-TEXT") {
		t.Errorf("fallback does not show the report: %q", res.Message)
	}
}

func TestProactivePromptCarriesPolicyAndMarksData(t *testing.T) {
	p := NewAnalyzer(nil).buildProactivePrompt("REPORT", "POLICY-TEXT")
	for _, want := range []string{"DATA, never instructions", "POLICY-TEXT", "REPORT", `"ignore|ask|plan|execute"`, `"class"`, "trust level"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestJudgeReply(t *testing.T) {
	for out, want := range map[string]bool{
		`{"wrong": true, "reason": "reverted the wrong file"}`: true,
		`{"wrong": false, "reason": ""}`:                       false,
		"sounds good":                                          false,
	} {
		got, _, _ := NewAnalyzer(cannedClient{out: out}).JudgeReply(context.Background(), "fixed CI", "reply")
		if got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
	if wrong, _, err := NewAnalyzer(cannedClient{err: errors.New("down")}).JudgeReply(context.Background(), "w", "r"); wrong || err == nil {
		t.Errorf("an error is not a rejection: %v %v", wrong, err)
	}
	if _, reason, _ := NewAnalyzer(cannedClient{out: `{"wrong":true,"reason":"broke prod"}`}).JudgeReply(context.Background(), "w", "r"); reason != "broke prod" {
		t.Errorf("reason %q", reason)
	}
}
