package chatcmd

import (
	"context"
	"strings"
	"time"
)

const (
	cmdProposals     = "/proposals"
	cmdProposal      = "/proposal"
	cmdRetrospective = "/retrospective"
)

// Proposals is the proposals' review surface (internal/proposals).
type Proposals interface {
	List() string
	Show(id string, full bool) (string, error)
	Approve(id, by string) (string, error)
	Reject(id, by, reason string) (string, error)
	Undo(id, by string) (string, error)
}

// SetProposals connects /proposals and /proposal to review.
func (c *Commander) SetProposals(p Proposals) { c.proposals = p }

// handleProposals is /proposals: the ones waiting for review.
func (c *Commander) handleProposals() string {
	if c.proposals == nil {
		return "Proposals are not available in this process."
	}
	reply := c.proposals.List()
	if !c.cfg.Agent.Proposals {
		reply += "\n\nAGENT_PROPOSALS is off: sessions cannot propose."
	}
	return reply
}

// handleProposal is /proposal show|approve|reject <id> [...]. Commands are
// admins' only, so whoever sends one may decide.
func (c *Commander) handleProposal(arg string) string {
	if c.proposals == nil {
		return "Proposals are not available in this process."
	}
	sub, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	id, more, _ := strings.Cut(strings.TrimSpace(rest), " ")
	more = strings.TrimSpace(more)
	var (
		reply string
		err   error
	)
	switch strings.ToLower(sub) {
	case "show":
		reply, err = c.proposals.Show(id, strings.EqualFold(more, "full"))
	case "approve", "apply":
		reply, err = c.proposals.Approve(id, "an admin (chat command)")
	case "reject":
		reply, err = c.proposals.Reject(id, "an admin (chat command)", more)
	case "undo", "revert":
		reply, err = c.proposals.Undo(id, "an admin (chat command)")
	case "", "help":
		return proposalHelp
	default:
		return "error: unknown " + cmdProposal + " command.\n\n" + proposalHelp
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return reply
}

// handleRetrospective is /retrospective [now].
func (c *Commander) handleRetrospective(arg string) string {
	if c.proactive == nil {
		return "The retrospective is not available in this process."
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "status":
		return c.proactive.RetroStatus()
	case "now", "run":
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		o := c.proactive.Retrospective(ctx)
		if o.Result == "started" {
			return "Retrospective started in " + c.cfg.Agent.ProactiveChat + ": the report and any proposals follow there."
		}
		return "**Retrospective:** " + o.String()
	case "on", "off":
		return c.handleConfigSet("AGENT_RETROSPECTIVE " + strings.ToLower(strings.TrimSpace(arg)))
	case "help":
		return retrospectiveHelp
	}
	return "error: unknown " + cmdRetrospective + " command.\n\n" + retrospectiveHelp
}

const proposalHelp = `**Proposals.** A session may propose a change to agent.md, prompt.md, GOALS.md or a skill of
its own (skills/<name>/SKILL.md in the memory dir). Each is posted in AGENT_PROACTIVE_CHAT, where a
yes or no in its thread decides it, and nothing changes until an admin approves. An approved one is
written as an operator edit (a memory commit of its own). A rejection's reason goes into
lessons.md so it is not proposed again. Unreviewed ones are reminded after 3 days and expire after 14.

  /proposals                         waiting for review, and the latest decided
  /proposal show <id> [full]         the change and why (full: the whole diff)
  /proposal approve <id>             apply it
  /proposal reject <id> [reason]     close it
  /proposal undo <id>                put the file back as it was before it was applied

Off switch: /config set AGENT_PROPOSALS off`

const retrospectiveHelp = `**Weekly retrospective.** Once a week a session reviews the week's sessions (daily logs, failures,
reviewer issues, rejected proposals) and posts a report in AGENT_PROACTIVE_CHAT, with at most 3
proposals to change the agent's instructions, each for your yes or no. It does no other work.

  /retrospective             status: on/off, when, the last one
  /retrospective on | off    AGENT_RETROSPECTIVE
  /retrospective now         run one now

When: retrospective: mon 08:00 in PROACTIVE.md (/proactive policy), in its timezone.`
