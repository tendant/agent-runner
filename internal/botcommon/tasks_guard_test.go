package botcommon

import (
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/task"
)

// A task whose turn has not ended (working, or cancelled while it runs)
// holds its workspace: nothing new may start or release it.
func TestLiveTurn(t *testing.T) {
	rec := &task.Record{Status: task.StatusWorking, Turns: []task.Turn{{SessionID: "s1", StartedAt: time.Now()}}}
	if liveTurn(rec) != "s1" {
		t.Error("working turn not ended: live")
	}
	rec.Status = task.StatusCancelled
	if liveTurn(rec) != "s1" {
		t.Error("cancelled while running: still live until it ends")
	}
	rec.Turns[0].EndedAt = time.Now()
	if liveTurn(rec) != "" {
		t.Error("an ended turn is not live")
	}
	rec = &task.Record{Status: task.StatusPaused, Turns: []task.Turn{{SessionID: "s2"}}}
	if liveTurn(rec) != "" {
		t.Error("a paused task has no live turn")
	}
}
