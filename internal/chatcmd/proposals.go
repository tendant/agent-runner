package chatcmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/evals"
)

const (
	cmdEvals         = "/evals"
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
	Evaluate(id string) (string, error)
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
	case "eval":
		reply, err = c.proposals.Evaluate(id)
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
  /proposal eval <id>                replay the eval set (/evals) with and without it, and compare

Off switch: /config set AGENT_PROPOSALS off`

const retrospectiveHelp = `**Weekly retrospective.** Once a week a session reviews the week's sessions (daily logs, failures,
reviewer issues, rejected proposals) and posts a report in AGENT_PROACTIVE_CHAT, with at most 3
proposals to change the agent's instructions, each for your yes or no. It does no other work.

  /retrospective             status: on/off, when, the last one
  /retrospective on | off    AGENT_RETROSPECTIVE
  /retrospective now         run one now

When: retrospective: mon 08:00 in PROACTIVE.md (/proactive policy), in its timezone.`

// handleEvals is /evals [show <name> | set <name> <content> | remove <name>]:
// the eval set in MEMORY_DIR/evals, the operator's.
func (c *Commander) handleEvals(arg string) string {
	sub, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	name, content, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if n, more, ok := strings.Cut(strings.TrimSpace(rest), "\n"); ok && !strings.Contains(n, " ") {
		name, content = n, more // set <name>\n<content>
	}
	name = strings.TrimSpace(name)
	switch strings.ToLower(sub) {
	case "", "list":
		set, err := evals.List(c.cfg.MemoryDir)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(set) == 0 {
			return "The eval set is empty.\n\n" + evalsHelp
		}
		var b strings.Builder
		b.WriteString("**Eval set** (" + evals.Dir + "/ in the memory dir)\n\n")
		for _, e := range set {
			fmt.Fprintf(&b, "- **%s**: %d check(s) · %s\n", e.Name, len(e.Checks), oneLine(e.Message, 80))
			for _, p := range e.Problems {
				b.WriteString("  - " + p + "\n")
			}
		}
		return b.String() + "\n/proposal eval <id> compares a proposal on it."
	case "show":
		path, err := evals.Path(c.cfg.MemoryDir, name)
		if err != nil {
			return "error: " + err.Error()
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "error: no eval " + name
		}
		return "**" + name + "**\n\n```\n" + strings.TrimSpace(string(data)) + "\n```"
	case "set":
		if name == "" || strings.TrimSpace(content) == "" {
			return "error: usage: " + cmdEvals + " set <name> <content>\n\n" + evalsHelp
		}
		e, err := evals.Write(c.cfg.MemoryDir, name, content)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("ok saved eval %s (%d check(s))", e.Name, len(e.Checks))
	case "remove", "rm", "delete":
		ok, err := evals.Remove(c.cfg.MemoryDir, name)
		switch {
		case err != nil:
			return "error: " + err.Error()
		case !ok:
			return "error: no eval " + name
		}
		return "ok removed eval " + name
	case "help":
		return evalsHelp
	}
	return "error: unknown " + cmdEvals + " command.\n\n" + evalsHelp
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

const evalsHelp = `**Eval set.** Past tasks you choose, each a message and checks, replayed to compare a proposal
with the current instructions (/proposal eval <id>). A replay runs sandboxed and keeps nothing: no
memory, pushes, files or messages. Replays count against AGENT_PROACTIVE_BUDGET_USD.

  /evals                          the set
  /evals show <name>
  /evals set <name> <content>     add or replace one (at most 10)
  /evals remove <name>

An eval:

  ---
  file: hello.go                  must exist in the workspace afterwards
  command: go vet ./...           must exit 0 there (in the replay's sandbox)
  reply: gofmt                    must appear in the agent's reply
  ---
  Add hello.go that prints hello, and commit it.`
