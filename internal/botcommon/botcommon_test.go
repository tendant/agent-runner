package botcommon

import (
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
)

func TestIsConfirmation(t *testing.T) {
	positives := []string{"yes", "Yes", "YES", "y", "ok", "sure", "proceed", "go", "do it", "confirm", "yep", "yeah"}
	for _, s := range positives {
		if !IsConfirmation(s) {
			t.Errorf("expected %q to be a confirmation", s)
		}
	}

	negatives := []string{"no", "maybe", "hello", "what", ""}
	for _, s := range negatives {
		if IsConfirmation(s) {
			t.Errorf("expected %q not to be a confirmation", s)
		}
	}
}

func TestIsDenial(t *testing.T) {
	positives := []string{"no", "No", "NO", "n", "nope", "cancel", "stop", "nah", "nevermind", "never mind"}
	for _, s := range positives {
		if !IsDenial(s) {
			t.Errorf("expected %q to be a denial", s)
		}
	}

	negatives := []string{"yes", "maybe", "hello", ""}
	for _, s := range negatives {
		if IsDenial(s) {
			t.Errorf("expected %q not to be a denial", s)
		}
	}
}

func TestFormatStatusLine_Completed(t *testing.T) {
	session := &agent.Session{
		Status:         agent.SessionStatusCompleted,
		Iterations:     make([]agent.IterationResult, 5),
		ElapsedSeconds: 120,
	}
	got := FormatStatusLine(session)
	want := "Session completed — 5 iterations in 120s"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatStatusLine_FailedWithError(t *testing.T) {
	session := &agent.Session{
		Status: agent.SessionStatusFailed,
		Error:  "aborted after 5 consecutive failures",
	}
	got := FormatStatusLine(session)
	want := "Session failed — aborted after 5 consecutive failures"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatStatusLine_FailedNoError(t *testing.T) {
	session := &agent.Session{Status: agent.SessionStatusFailed}
	got := FormatStatusLine(session)
	want := "Session failed"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatStatusLine_NoWarningsSuffix(t *testing.T) {
	// FormatStatusLine must never include the warnings line itself — callers
	// control where warnings land relative to their own platform-specific extras.
	session := &agent.Session{Status: agent.SessionStatusCompleted, Warnings: []string{"missing API key"}}
	if got := FormatStatusLine(session); strings.Contains(got, "⚠") {
		t.Errorf("FormatStatusLine must not include warnings, got: %q", got)
	}
}

func TestFormatWarningsSuffix_None(t *testing.T) {
	if got := FormatWarningsSuffix(&agent.Session{}); got != "" {
		t.Errorf("expected empty suffix with no warnings, got %q", got)
	}
}

func TestFormatWarningsSuffix_Joined(t *testing.T) {
	session := &agent.Session{Warnings: []string{"missing API key", "shared repo not found"}}
	got := FormatWarningsSuffix(session)
	want := "\n\n⚠ missing API key; shared repo not found"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIterationCore_SuccessWithCommit(t *testing.T) {
	iter := agent.IterationResult{Iteration: 3, Status: agent.IterationStatusSuccess, Commit: "abc123"}

	md := FormatIterationCore(iter, true)
	if !strings.Contains(md, "(commit `abc123`)") {
		t.Errorf("expected backtick-wrapped commit with markdownCommit=true, got: %q", md)
	}

	plain := FormatIterationCore(iter, false)
	if !strings.Contains(plain, "(commit abc123)") || strings.Contains(plain, "`") {
		t.Errorf("expected plain commit with markdownCommit=false, got: %q", plain)
	}
}

func TestFormatIterationCore_SuccessNoCommit(t *testing.T) {
	iter := agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess}
	got := FormatIterationCore(iter, true)
	want := "Iteration 1: completed"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIterationCore_NoChanges(t *testing.T) {
	iter := agent.IterationResult{Iteration: 2, Status: agent.IterationStatusNoChanges}
	got := FormatIterationCore(iter, true)
	want := "Iteration 2: no changes"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIterationCore_ValidationFailed(t *testing.T) {
	iter := agent.IterationResult{Iteration: 4, Status: agent.IterationStatusValidation, Error: "blocked path"}
	got := FormatIterationCore(iter, true)
	want := "Iteration 4: validation failed — blocked path"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIterationCore_Error(t *testing.T) {
	iter := agent.IterationResult{Iteration: 5, Status: agent.IterationStatusError, Error: "timeout"}
	got := FormatIterationCore(iter, true)
	want := "Iteration 5: error — timeout"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatIterationCore_TruncatesLongOutput(t *testing.T) {
	iter := agent.IterationResult{Iteration: 1, Status: agent.IterationStatusSuccess, Output: strings.Repeat("x", 5000)}
	got := FormatIterationCore(iter, true)
	if !strings.Contains(got, "... (truncated)") {
		t.Error("expected truncation marker for output over 3000 chars")
	}
	if strings.Contains(got, strings.Repeat("x", 3001)) {
		t.Error("output should be capped at 3000 chars before the truncation marker")
	}
}
