package thread

import (
	"slices"
	"testing"
)

// Messages queued during a run are all kept; a steered one is passed on
// without dropping the others.
func TestPendingMessages(t *testing.T) {
	c := &Thread{State: StateExecuting}
	c.AddMessage("user", "also add tests")
	c.AddMessage("user", "and update the docs")
	c.TakePendingInput() // the latest was steered into the running agent
	if got := c.PendingMessages(); !slices.Equal(got, []string{"also add tests"}) {
		t.Errorf("pending %v", got)
	}
	if !c.ClearPendingInput() || c.ClearPendingInput() {
		t.Error("clear reports what was queued, once")
	}
}
