package proactive

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The weekly retrospective (PROACTIVE_DESIGN.md §10): a session that looks
// back over the week's sessions and proposes changes to the agent's own
// instructions, with at least two pieces of evidence each. It writes
// proposals, a report and memory, and runs no other work.

// retroPeriod is how far back a first retrospective looks.
const retroPeriod = 7 * 24 * time.Hour

// retroDue reports whether the retrospective's slot has come since the last
// one (or since the schedule was first seen on, so turning it on waits for
// the next slot).
func (s *Service) retroDue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	now := s.cfg.Now()
	from := st.LastRetro
	if from.IsZero() {
		if st.RetroSince.IsZero() {
			st.RetroSince = now
			s.save(st)
			return false
		}
		from = st.RetroSince
	}
	return !now.Before(s.policy().NextRetro(from))
}

// Retrospective runs one now: it opens a thread in AGENT_PROACTIVE_CHAT and
// starts the review session there. It is recorded before the session starts,
// so a crash cannot repeat it.
func (s *Service) Retrospective(ctx context.Context) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	set := s.cfg.Settings()
	st := s.load()
	switch {
	case set.Chat == "":
		return Outcome{At: now, Result: "skipped", Reason: "AGENT_PROACTIVE_CHAT is not set: the retrospective has nowhere to report"}
	case s.cfg.Chat == nil:
		return Outcome{At: now, Result: "skipped", Reason: "no chat configured"}
	}
	since := st.LastRetro
	if since.IsZero() || now.Sub(since) > 2*retroPeriod {
		since = now.Add(-retroPeriod)
	}
	st.LastRetro = now
	pol := s.policy()
	intro := fmt.Sprintf("**Weekly retrospective** (%s – %s)\n\nLooking back over the week's sessions for patterns worth a change to the agent's instructions. The report follows in this thread; each proposal is posted on its own for your yes or no.",
		pol.in(since).Format("Jan 2"), pol.in(now).Format("Jan 2"))
	_, _, err := s.cfg.Chat.StartProactiveRun(ctx, set.Chat, intro, s.retroMessage(since, now, pol))
	o := Outcome{At: now, Result: "started"}
	if err != nil {
		o = Outcome{At: now, Result: "error", Reason: err.Error()}
	}
	st.RetroNote = o.String()
	s.save(st)
	return o
}

// retroMessage is the retrospective session's request.
func (s *Service) retroMessage(since, now time.Time, pol Policy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Weekly retrospective, started by agent-runner on its schedule, not by a person.]\n\n")
	fmt.Fprintf(&b, "Review how you worked from %s to %s, and propose improvements to your own instructions.\n\n",
		pol.in(since).Format("2006-01-02 15:04"), pol.in(now).Format("2006-01-02 15:04 MST"))
	b.WriteString("Read, in your memory directory:\n\n" +
		"- the daily logs of this period (files named `YYYY-MM-DD-<host>.md`): each session's request, outcome,\n" +
		"  reviewer score and issues\n" +
		"- `lessons.md`, including the proposals people rejected and why: never propose those again\n" +
		"- `" + AgendaFile + "`: open items without a due date; mark done the ones that are, and list the rest\n" +
		"  in your report\n" +
		"- `" + GoalsFile + "`, if present: the operator's goals\n")

	var failed []string
	if s.cfg.Sessions != nil {
		for _, ss := range s.cfg.Sessions() {
			if ss.Ended.Before(since) || ss.Status == "completed" {
				continue
			}
			line := fmt.Sprintf("- %s (%s, %s, %s)", ss.ID, ss.Source, ss.Status, pol.in(ss.Ended).Format("Jan 2 15:04"))
			if ss.Error != "" {
				line += ": " + clip(ss.Error, 200)
			}
			failed = append(failed, line)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\nSessions that failed or were stopped in this period (since agent-runner last started):\n\n" + strings.Join(failed[:min(len(failed), 30)], "\n") + "\n")
	}
	if s.cfg.Proposals != nil {
		if lines := s.cfg.Proposals(since); len(lines) > 0 {
			b.WriteString("\nProposals made or decided in this period:\n\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	b.WriteString("\nThen:\n\n" +
		"1. Find patterns, each backed by at least two sessions: the same failure again, the same correction\n" +
		"   from people, a procedure explained more than once, a reviewer issue that keeps coming back.\n" +
		"2. For each pattern an instruction change would fix, write a proposal in `_proposals/` as your\n" +
		"   instructions describe: at most 3, the best-founded first, with the sessions as evidence. A pattern\n" +
		"   too weak for a proposal goes in the report instead.\n" +
		"3. Reply with a short report: what went well, what failed repeatedly, what you propose and why,\n" +
		"   and the open agenda items.\n\n" +
		"This is a review only: do not change, commit or push anything in the repos, and do not start\n" +
		"other work.\n")
	return b.String()
}

// RetroStatus describes the retrospective for /retrospective.
func (s *Service) RetroStatus() string {
	set := s.cfg.Settings()
	pol := s.policy()
	st := s.load()
	var b strings.Builder
	onOff := "off"
	if set.Retrospective {
		onOff = "on"
	}
	fmt.Fprintf(&b, "**Weekly retrospective: %s** (AGENT_RETROSPECTIVE)\n\n", onOff)
	fmt.Fprintf(&b, "- when: %s %02d:%02d (%s retrospective:)\n", pol.RetroDay, pol.RetroAt/60, pol.RetroAt%60, PolicyFile)
	if set.Chat == "" {
		b.WriteString("- chat: not set (AGENT_PROACTIVE_CHAT): it cannot run\n")
	} else {
		fmt.Fprintf(&b, "- reports in: %s\n", set.Chat)
	}
	if !st.LastRetro.IsZero() {
		fmt.Fprintf(&b, "- last: %s, %s\n", pol.in(st.LastRetro).Format("2006-01-02 15:04"), st.RetroNote)
	}
	if set.Retrospective {
		from := st.LastRetro
		if from.IsZero() {
			from = st.RetroSince
		}
		if from.IsZero() {
			from = s.cfg.Now()
		}
		fmt.Fprintf(&b, "- next: %s\n", pol.NextRetro(from).Format("Mon 2006-01-02 15:04"))
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
