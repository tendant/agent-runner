package botcommon

import (
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// These are vars, not consts, so tests can shrink them to avoid waiting on
// real 5-second ticks / hour-long deadlines. Production callers never
// override them, so the effective values are always the defaults below.

// pollInterval is how often PollAndReport checks the session for updates.
var pollInterval = 5 * time.Second

// fallbackTimeout is the deadline used when a session has no MaxTotalSeconds set.
var fallbackTimeout = 2 * time.Hour

// deadlineGrace is added on top of a session's MaxTotalSeconds so the poll
// loop's own timeout fires after the session's internal deadline, not before it.
var deadlineGrace = 30 * time.Second

// Reporter receives callbacks from PollAndReport as a session progresses.
// Implementations close over their own chat/conversation/user ID and their
// own send primitive — PollAndReport itself is agnostic to both.
type Reporter interface {
	// OnIterationComplete is called once per newly completed iteration, in
	// order. Bots that report progress per finished iteration (telegram,
	// wechat) implement this; others can leave it a no-op.
	OnIterationComplete(iter agent.IterationResult)
	// OnIterationStart is called when session.CurrentIteration advances,
	// i.e. a new iteration has begun (before its result is available). Bots
	// that report progress per started iteration (stream) implement this;
	// others can leave it a no-op.
	OnIterationStart(current, max int)
	// OnFinal is called exactly once, when the session reaches a terminal status.
	OnFinal(session *agent.Session)
	// OnNotFound is called if the session ID can't be found.
	OnNotFound()
	// OnTimeout is called if the session exceeds its deadline without
	// reaching a terminal status.
	OnTimeout()
}

// PollAndReport polls sessionID every 5s until it reaches a terminal status,
// times out, or is not found, dispatching to r. The deadline is derived from
// session.MaxTotalSeconds (+30s grace), falling back to 2h if unset.
//
// On each tick, newly completed iterations and a newly started iteration are
// both reported (in that order) before the terminal-status check, so a
// session's last iteration is always reported before its final result.
func PollAndReport(starter AgentStarter, sessionID string, r Reporter) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	reportedIterations := 0 // number of completed iterations already reported
	reportedCurrent := 0    // highest CurrentIteration already reported as started

	var deadline <-chan time.Time

	for {
		select {
		case <-ticker.C:
			session, exists := starter.GetAgentSession(sessionID)
			if !exists {
				r.OnNotFound()
				return
			}

			// Set deadline on first successful lookup.
			if deadline == nil {
				maxSecs := session.MaxTotalSeconds
				if maxSecs <= 0 {
					maxSecs = int(fallbackTimeout.Seconds())
				}
				t := time.NewTimer(time.Duration(maxSecs)*time.Second + deadlineGrace)
				defer t.Stop()
				deadline = t.C
			}

			for i := reportedIterations; i < len(session.Iterations); i++ {
				r.OnIterationComplete(session.Iterations[i])
			}
			reportedIterations = len(session.Iterations)

			if cur := session.CurrentIteration; cur > reportedCurrent {
				reportedCurrent = cur
				r.OnIterationStart(cur, session.MaxIterations)
			}

			if session.Status == agent.SessionStatusCompleted || session.Status == agent.SessionStatusFailed {
				r.OnFinal(session)
				return
			}

		case <-deadline:
			r.OnTimeout()
			return
		}
	}
}
