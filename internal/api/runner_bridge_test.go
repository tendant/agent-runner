package api

import (
	"testing"

	"github.com/agent-runner/agent-runner/internal/agent"
)

func TestLastIterationOutput_ReturnsLastNonEmpty(t *testing.T) {
	snap := &agent.Session{
		Iterations: []agent.IterationResult{
			{Iteration: 1, Output: "first output"},
			{Iteration: 2, Output: ""},
			{Iteration: 3, Output: "third output"},
		},
	}

	result := lastAgentOutput(snap)
	if result != "third output" {
		t.Errorf("expected 'third output', got %q", result)
	}
}

func TestLastIterationOutput_AllEmpty(t *testing.T) {
	snap := &agent.Session{
		Iterations: []agent.IterationResult{
			{Iteration: 1, Output: ""},
			{Iteration: 2, Output: ""},
		},
	}

	result := lastAgentOutput(snap)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestLastIterationOutput_NilIterations(t *testing.T) {
	snap := &agent.Session{}

	result := lastAgentOutput(snap)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestLastIterationOutput_SingleIteration(t *testing.T) {
	snap := &agent.Session{
		Iterations: []agent.IterationResult{
			{Iteration: 1, Output: "only output"},
		},
	}

	result := lastAgentOutput(snap)
	if result != "only output" {
		t.Errorf("expected 'only output', got %q", result)
	}
}

func TestLastIterationOutput_SkipsMiddleEmpty(t *testing.T) {
	snap := &agent.Session{
		Iterations: []agent.IterationResult{
			{Iteration: 1, Output: "first"},
			{Iteration: 2, Output: ""},
			{Iteration: 3, Output: ""},
		},
	}

	result := lastAgentOutput(snap)
	if result != "first" {
		t.Errorf("expected 'first', got %q", result)
	}
}
