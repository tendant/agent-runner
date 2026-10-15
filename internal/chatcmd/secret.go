package chatcmd

import (
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

// handleSecret manages the agent user's secrets (internal/agentsecrets):
// the credentials agents use for their tasks, apart from the runner's own
// (/set). Replies never repeat a value.
func (c *Commander) handleSecret(arg string) string {
	store := agentsecrets.New(c.cfg.SecretsFile)
	fields := strings.Fields(arg)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "", "list":
		list, err := store.List()
		if err != nil {
			return "error: " + err.Error()
		}
		if len(list) == 0 {
			return "no agent secrets (add one: /secret set NAME VALUE [description])"
		}
		var b strings.Builder
		b.WriteString("Agent secrets (in every session's environment):\n")
		for _, s := range list {
			fmt.Fprintf(&b, "- %s", s.Name)
			if s.Description != "" {
				fmt.Fprintf(&b, ": %s", s.Description)
			}
			b.WriteString("\n")
		}
		return strings.TrimSpace(b.String())
	case "set":
		// One line: NAME VALUE [description]. A value of several lines (a
		// private key) goes on the lines after: NAME [description] first.
		head, body, multiline := strings.Cut(arg, "\n")
		hf := strings.Fields(head)
		var name, value, desc string
		switch {
		case multiline && strings.TrimSpace(body) != "" && len(hf) >= 2:
			name, value, desc = hf[1], strings.TrimSpace(body), strings.Join(hf[2:], " ")
		case len(fields) >= 3 && !multiline:
			name, value, desc = fields[1], fields[2], strings.Join(fields[3:], " ")
		default:
			return "error: usage: /secret set NAME VALUE [description], or for a value of several lines (a private key): /secret set NAME [description] and the value on the lines after"
		}
		if err := store.Set(name, value, desc); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("ok: %s set (%d characters); agents get it as $%s from their next session", name, len(value), name)
	case "remove", "rm", "delete":
		if len(fields) != 2 {
			return "error: usage: /secret remove NAME"
		}
		ok, err := store.Remove(fields[1])
		if err != nil {
			return "error: " + err.Error()
		}
		if !ok {
			return "no secret named " + fields[1]
		}
		return "ok: " + fields[1] + " removed"
	}
	return "error: usage: /secret [list|set NAME VALUE [description]|remove NAME]"
}
