package chatcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/proactive"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

const (
	cmdProactive = "/proactive"
	cmdGoals     = "/goals"
	cmdAgenda    = "/agenda"
	cmdTrust     = "/trust"
)

// Proactive is the proactive checks' surface the chat commands use.
type Proactive interface {
	Status() string
	Agenda() string
	Tick(ctx context.Context, force bool) proactive.Outcome
	RetroStatus() string
	Retrospective(ctx context.Context) proactive.Outcome
	TrustStatus() string
	SetTrust(class, level string) (string, error)
	SessionUndone(ctx context.Context, sessionID string) string
}

// handleTrust is /trust [<class> <level>]: the action classes' levels.
// Raising one is the operator's alone (commands are admins' only).
func (c *Commander) handleTrust(arg string) string {
	if c.proactive == nil {
		return "Trust levels are not available in this process."
	}
	f := strings.Fields(arg)
	switch len(f) {
	case 0:
		return c.proactive.TrustStatus()
	case 2:
		reply, err := c.proactive.SetTrust(f[0], f[1])
		if err != nil {
			return "error: " + err.Error()
		}
		return reply
	}
	return "error: usage: " + cmdTrust + " [<class> off|propose|notify|quiet]"
}

// SetProactive connects /proactive and /agenda to the running checks.
func (c *Commander) SetProactive(p Proactive) { c.proactive = p }

// handleProactive is /proactive [on|off|tick|policy ...|help].
func (c *Commander) handleProactive(arg string) string {
	sub, rest, _ := strings.Cut(arg, " ")
	switch strings.ToLower(sub) {
	case "", "status":
		if c.proactive == nil {
			return "Proactive checks are not available in this process."
		}
		return c.proactive.Status()
	case "on", "off":
		reply := c.handleConfigSet("AGENT_PROACTIVE " + strings.ToLower(sub))
		if strings.HasPrefix(reply, "error") {
			return reply
		}
		if strings.ToLower(sub) == "on" && c.cfg.Agent.ProactiveChat == "" {
			reply += "\n\nSet where proactive messages go: `/config set AGENT_PROACTIVE_CHAT <channel id>` (or `telegram`)."
		}
		return reply
	case "tick", "check":
		if c.proactive == nil {
			return "Proactive checks are not available in this process."
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		o := c.proactive.Tick(ctx, true)
		msg := "**Proactive check:** " + o.String()
		if o.Message != "" && o.Result != "ask" && o.Result != "plan" {
			msg += "\n\n" + o.Message
		}
		if o.Result == "ask" || o.Result == "plan" {
			msg += " — posted in " + c.cfg.Agent.ProactiveChat
		}
		return msg
	case "policy":
		return c.handleOperatorFile(proactive.PolicyFile, cmdProactive+" policy", strings.TrimSpace(rest), proactivePolicyHelp)
	case "help":
		return proactiveHelp
	}
	return "error: unknown " + cmdProactive + " command.\n\n" + proactiveHelp
}

// handleGoals is /goals [set <text>|clear].
func (c *Commander) handleGoals(arg string) string {
	return c.handleOperatorFile(proactive.GoalsFile, cmdGoals, arg, goalsHelp)
}

// handleAgenda is /agenda: the agent's open loops, due ones first.
func (c *Commander) handleAgenda() string {
	if c.proactive == nil {
		return "Proactive checks are not available in this process."
	}
	return c.proactive.Agenda()
}

// handleOperatorFile shows, sets or clears an operator-only file in the
// memory dir (upper case, so sessions cannot edit it), as /alerts does.
func (c *Commander) handleOperatorFile(name, cmd, arg, help string) string {
	path := filepath.Join(c.cfg.MemoryDir, name)
	sub, rest, _ := strings.Cut(arg, " ")
	switch strings.ToLower(strings.TrimSpace(sub)) {
	case "":
		data, _ := os.ReadFile(path)
		if text := strings.TrimSpace(string(data)); text != "" {
			return "**" + name + "**\n\n" + text
		}
		return help
	case "set":
		text := strings.TrimSpace(rest)
		if text == "" {
			return "error: usage: " + cmd + " set <text>\n\n" + help
		}
		if err := tmpl.WriteInstructions(c.cfg.MemoryDir, path, text+"\n", "memory: "+name+" edited by the operator (chat)"); err != nil {
			return fmt.Sprintf("error: write %s: %v", name, err)
		}
		reply := fmt.Sprintf("ok wrote %s (%d bytes)", name, len(text))
		if name == proactive.PolicyFile {
			if p := proactive.ParsePolicy(text); len(p.Problems) > 0 {
				reply += "\n\nSettings it could not read (ignored):\n- " + strings.Join(p.Problems, "\n- ")
			}
		}
		return reply
	case "clear":
		if err := tmpl.WriteInstructions(c.cfg.MemoryDir, path, "", "memory: "+name+" cleared by the operator (chat)"); err != nil {
			return fmt.Sprintf("error: write %s: %v", name, err)
		}
		return "ok cleared " + name
	case "help":
		return help
	}
	return "error: unknown " + cmd + " command.\n\n" + help
}

const proactiveHelp = `**Proactive checks.** Every AGENT_PROACTIVE_INTERVAL (30m) the agent looks at its due agenda
items (` + "`agenda.md`" + `, which its sessions keep), sessions that failed with nobody watching, and the
signals PROACTIVE.md turns on (repo CI, new commits). When something needs attention it posts a note,
or a proposal that waits for your yes, in AGENT_PROACTIVE_CHAT. Work runs without a yes only for an
action class you raised with /trust (and AGENT_SANDBOX=strict). A check with nothing new makes no model call.

  /proactive               status, today's counts, the last check
  /proactive on | off      the kill switch (AGENT_PROACTIVE)
  /proactive tick          check now, even when off or recently seen
  /proactive policy [set <text> | clear]   cadence, quiet hours, caps, what to raise (PROACTIVE.md)
  /goals [set <text> | clear]              standing goals (GOALS.md), in every session's prompt too
  /agenda                  the agent's open loops
  /trust [<class> <level>] what each action class may do without asking

Where messages go: /config set AGENT_PROACTIVE_CHAT <agent-stream channel id, or telegram>`

const proactivePolicyHelp = `**Proactive policy** (PROACTIVE.md): optional settings in front matter, then what to raise,
propose or ignore, in your words. Settings may lengthen the interval and lower the caps, never
the reverse.

  /proactive policy set ---
  interval: 1h
  timezone: America/Los_Angeles
  quiet_hours: 22:00-07:00
  max_posts_per_day: 5
  retrospective: mon 08:00
  signals: repo-ci, repo-commits
  max_sessions_per_day: 3
  ---
  Failed scheduled jobs: propose a fix. Due deploy checks: propose checking. Anything else: a short note.

  # Action classes
  - ci-fix: CI failing on a watched repo's default branch. Investigate, fix, push.
    trust: propose

A class's trust: is only where it starts; /trust changes it.`

const goalsHelp = `**Goals** (GOALS.md): standing objectives, in your words. Every session sees them, and the
proactive checks weigh what they find against them.

  /goals set Keep CI green on main in agent-runner. Make sure the nightly backup job succeeds.`
