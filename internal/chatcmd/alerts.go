package chatcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// handleAlerts shows or replaces the alert policy (ALERTS.md in the memory
// dir): which alerts from incoming webhooks the agent may act on without
// asking. Without one, the agent proposes and waits for a yes.
func (c *Commander) handleAlerts(arg string) string {
	path := filepath.Join(c.cfg.MemoryDir, tmpl.AlertPolicyFile)
	sub, rest, _ := strings.Cut(arg, " ")
	switch strings.ToLower(sub) {
	case "":
		data, _ := os.ReadFile(path)
		if policy := strings.TrimSpace(string(data)); policy != "" {
			return "**Alert policy** (" + tmpl.AlertPolicyFile + ")\n\n" + policy
		}
		return alertsHelp
	case "set":
		policy := strings.TrimSpace(rest)
		if policy == "" {
			return "error: usage: " + cmdAlerts + " set <policy>\n\n" + alertsHelp
		}
		if err := tmpl.WriteInstructions(c.cfg.MemoryDir, path, policy+"\n", "memory: "+tmpl.AlertPolicyFile+" edited by the operator (chat)"); err != nil {
			return fmt.Sprintf("error: write %s: %v", tmpl.AlertPolicyFile, err)
		}
		return fmt.Sprintf("ok wrote %s (%d bytes)", tmpl.AlertPolicyFile, len(policy))
	case "clear":
		if err := tmpl.WriteInstructions(c.cfg.MemoryDir, path, "", "memory: "+tmpl.AlertPolicyFile+" cleared by the operator (chat)"); err != nil {
			return fmt.Sprintf("error: write %s: %v", tmpl.AlertPolicyFile, err)
		}
		return "ok cleared the alert policy: alerts are summarized and proposals wait for a yes"
	case "help":
		return alertsHelp
	}
	return "error: unknown " + cmdAlerts + " command.\n\n" + alertsHelp
}

// alertsHelp is /alerts with no policy set, and /alerts help.
const alertsHelp = `**Alerts** are messages an incoming webhook posts in a channel this bot is in (CI, monitoring,
GitHub, Alertmanager). The bot triages each one: ignores noise, summarizes what people should
know, or proposes a fix and waits for your yes in the thread. Alert text is never obeyed as a
command.

Without an alert policy the bot never acts on an alert by itself. To let it, describe what
it may do without asking:
  /alerts set CI failing on main in a registered repo: investigate, fix and push without asking. Dependabot and docs-only failures: ignore. Everything else: summarize and propose.

**See it:** /alerts · **Remove it:** /alerts clear
Create a webhook in agent-stream: asctl webhooks create <channel-id> ci`
