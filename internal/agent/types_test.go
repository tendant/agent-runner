package agent

import "testing"

func TestLastIterationError_NoIterations(t *testing.T) {
	s := &Session{}
	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 0 || errMsg != "" || partialOut != "" {
		t.Errorf("expected zero values, got (%d, %q, %q)", iterNum, errMsg, partialOut)
	}
}

func TestLastIterationError_LastSuccess(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusError,
		Error:     "something failed",
		Output:    "partial",
	})
	s.AddIteration(IterationResult{
		Iteration: 2,
		Status:    IterationStatusSuccess,
		Output:    "all good",
	})

	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 0 || errMsg != "" || partialOut != "" {
		t.Errorf("expected zero values after success, got (%d, %q, %q)", iterNum, errMsg, partialOut)
	}
}

func TestLastIterationError_LastError(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusSuccess,
	})
	s.AddIteration(IterationResult{
		Iteration: 2,
		Status:    IterationStatusError,
		Error:     "claude execution failed: timeout",
		Output:    "partial work here",
	})

	iterNum, errMsg, partialOut := s.LastIterationError()
	if iterNum != 2 {
		t.Errorf("expected iteration 2, got %d", iterNum)
	}
	if errMsg != "claude execution failed: timeout" {
		t.Errorf("unexpected error message: %q", errMsg)
	}
	if partialOut != "partial work here" {
		t.Errorf("unexpected partial output: %q", partialOut)
	}
}

func TestLastIterationError_ValidationFailure(t *testing.T) {
	s := &Session{}
	s.AddIteration(IterationResult{
		Iteration: 1,
		Status:    IterationStatusValidation,
		Error:     "validation failed",
	})

	iterNum, errMsg, _ := s.LastIterationError()
	if iterNum != 1 || errMsg != "validation failed" {
		t.Errorf("expected validation failure returned, got (%d, %q)", iterNum, errMsg)
	}
}
