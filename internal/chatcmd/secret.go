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
	case "help":
		return secretHelp
	case "", "list":
		list, err := store.List()
		if err != nil {
			return "error: " + err.Error()
		}
		if len(list) == 0 {
			return "no agent secrets yet.\n\n" + secretHelp
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
		case multiline && len(hf) >= 3 && !strings.Contains(strings.TrimSpace(body), "\n") && !strings.HasPrefix(strings.TrimSpace(body), "-----BEGIN"):
			// The value is on the first line and a note follows: the
			// note is description, never the value.
			name, value = hf[1], hf[2]
			desc = strings.TrimSpace(strings.Join(hf[3:], " ") + " " + strings.TrimSpace(body))
		case multiline && strings.TrimSpace(body) != "" && len(hf) >= 2:
			name, value, desc = hf[1], strings.TrimSpace(body), strings.Join(hf[2:], " ")
		case len(fields) >= 3 && !multiline:
			name, value, desc = fields[1], fields[2], strings.Join(fields[3:], " ")
		default:
			return "error: /secret set needs a name and a value.\n\n" + secretHelp
		}
		if err := store.Set(name, value, desc); err != nil {
			return "error: " + err.Error() + "\n(/secret help shows the format)"
		}
		return fmt.Sprintf("ok: %s set (%d characters). Agents get it as $%s from their next session; a repo uses it with /repo add <url> secret=%s.", name, len(value), name, name)
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
	return "error: unknown /secret command.\n\n" + secretHelp
}

// secretHelp is the guide /secret help (and an empty list, and a mistake)
// shows: what a secret is for, and the formats with examples.
const secretHelp = `**Secrets** are credentials your agents use for their tasks: API tokens, git tokens, deploy keys.
Every session gets each one as an environment variable ($NAME); agents see the name and your
description, and use the value. agent-runner's own keys (/set) are separate and never shared.

**Add or replace** — one line: name, value, then an optional description
  /secret set GITEE_TOKEN 1a2b3c4d Gitee API and git for oam-server
  /secret set GH_TOKEN ghp_xxx GitHub, read-only

**A value of several lines** (an SSH private key) — name and description on the first line,
the value on the lines after, in the same message:
  /secret set GITEE_SSH_KEY deploy key for oam
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...
  -----END OPENSSH PRIVATE KEY-----

**Use one for a repo:** /repo add https://<user>@gitee.com/org/repo.git secret=GITEE_TOKEN
**See them:** /secret list (names and descriptions, never values)
**Remove one:** /secret remove GITEE_TOKEN

Names: upper case, digits and _ — the names tools expect (GITEE_TOKEN, GH_TOKEN, STRIPE_API_KEY);
names agent-runner uses itself (GIT_*, AGENT_*, ...) are refused. Send credentials only with
/secret: a token in an ordinary message goes to the agent's model and its logs.`
