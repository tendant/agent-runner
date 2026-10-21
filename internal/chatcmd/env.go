package chatcmd

import (
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

// handleEnv manages the agent's environment (internal/agentsecrets): what
// the agent's tasks get, apart from agent-runner's own settings (/config).
// Two kinds: plain settings (/env set, shown with their values) and secrets
// (/env secret, never shown). Replies never repeat a secret's value.
func (c *Commander) handleEnv(arg string) string {
	secrets := agentsecrets.New(c.cfg.SecretsFile)
	plain := agentsecrets.New(c.cfg.EnvFile)
	fields := strings.Fields(arg)
	sub := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}
	switch sub {
	case "help":
		return envHelp
	case "", "list":
		return c.envList()
	case "set":
		head, _, multiline := strings.Cut(arg, "\n")
		if multiline {
			return "error: a plain setting is one line. A value of several lines (a key) is a secret: /env secret NAME [description], then the value on the lines after."
		}
		name, value, desc, ok := parsePlain(strings.TrimSpace(head[len(fields[0]):]))
		if !ok {
			return "error: /env set needs a name and a value.\n\n" + envHelp
		}
		if strings.Contains(value, sealedMask) {
			return errMaskedValue
		}
		if err := agentsecrets.CheckNameFor(name, c.cfg.Agent.CLI); err != nil {
			return "error: " + err.Error()
		}
		if agentsecrets.LooksLikeCredential(value) {
			// Never echo it: it may be a credential.
			return fmt.Sprintf("error: that value looks like a credential, and a plain setting is shown in chat, the prompt and logs: nothing was saved. Send it as a secret: /env secret %s <value> [description]", name)
		}
		if err := plain.Set(name, value, desc); err != nil {
			return "error: " + err.Error() + "\n(/env help shows the format)"
		}
		note := ""
		if was, _ := secrets.Remove(name); was {
			note = " (it was a secret; now plain)"
		}
		return fmt.Sprintf("agent's environment: %s=%s%s, from the next session.", name, value, note)
	case "secret":
		name, value, desc, errText := parseSecret(arg, fields)
		if errText != "" {
			return errText
		}
		if strings.Contains(value, sealedMask) {
			return errMaskedValue
		}
		if err := agentsecrets.CheckNameFor(name, c.cfg.Agent.CLI); err != nil {
			return "error: " + err.Error()
		}
		if err := secrets.Set(name, value, desc); err != nil {
			return "error: " + err.Error() + "\n(/env help shows the format)"
		}
		note := ""
		if was, _ := plain.Remove(name); was {
			note = " (it was a plain setting; now secret)"
		}
		return fmt.Sprintf("agent's secret: %s (%d characters, hidden)%s, as $%s from the next session; a repo logs in with it by /repo add <url> secret=%s.", name, len(value), note, name, name)
	case "unset", "remove", "rm", "delete":
		if len(fields) != 2 {
			return "error: usage: /env unset NAME"
		}
		name := fields[1]
		wasPlain, err := plain.Remove(name)
		if err != nil {
			return "error: " + err.Error()
		}
		wasSecret, err := secrets.Remove(name)
		if err != nil {
			return "error: " + err.Error()
		}
		if !wasPlain && !wasSecret {
			return "nothing named " + name + " in the agent's environment (/env lists it)"
		}
		return "ok: " + name + " removed from the agent's environment, from the next session"
	}
	return "error: unknown /env command.\n\n" + envHelp
}

// envList is /env: the agent's whole environment, plain settings with their
// values, secrets by name.
func (c *Commander) envList() string {
	plain, secrets, err := agentsecrets.Load(c.cfg.SecretsFile, c.cfg.EnvFile)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(plain)+len(secrets) == 0 {
		return "the agent's environment is empty.\n\n" + envHelp
	}
	var b strings.Builder
	b.WriteString("The agent's environment (every session gets it):\n")
	for _, v := range plain {
		fmt.Fprintf(&b, "- %s=%s", v.Name, v.Value)
		if v.Description != "" {
			fmt.Fprintf(&b, ": %s", v.Description)
		}
		b.WriteString("\n")
	}
	for _, s := range secrets {
		fmt.Fprintf(&b, "- %s=%s (secret)", s.Name, sealedMask)
		if s.Description != "" {
			fmt.Fprintf(&b, ": %s", s.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// parsePlain reads "NAME VALUE [description]"; a value with spaces is
// quoted ("hello world").
func parsePlain(s string) (name, value, desc string, ok bool) {
	name, rest, _ := strings.Cut(strings.TrimSpace(s), " ")
	rest = strings.TrimSpace(rest)
	if name == "" || rest == "" {
		return "", "", "", false
	}
	if q := rest[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", "", "", false
		}
		return name, rest[1 : end+1], strings.TrimSpace(rest[end+2:]), true
	}
	value, desc, _ = strings.Cut(rest, " ")
	return name, value, strings.TrimSpace(desc), true
}

// parseSecret reads "/env secret NAME VALUE [description]", or a value of
// several lines (a private key) after "NAME [description]" on the first.
func parseSecret(arg string, fields []string) (name, value, desc, errText string) {
	head, body, multiline := strings.Cut(arg, "\n")
	hf := strings.Fields(head)
	switch {
	case multiline && len(hf) >= 3 && !strings.Contains(strings.TrimSpace(body), "\n") && !strings.HasPrefix(strings.TrimSpace(body), "-----BEGIN"):
		// The value is on the first line and a note follows: the note is
		// description, never the value.
		return hf[1], hf[2], strings.TrimSpace(strings.Join(hf[3:], " ") + " " + strings.TrimSpace(body)), ""
	case multiline && strings.TrimSpace(body) != "" && len(hf) >= 2:
		return hf[1], strings.TrimSpace(body), strings.Join(hf[2:], " "), ""
	case len(fields) >= 3 && !multiline:
		return fields[1], fields[2], strings.Join(fields[3:], " "), ""
	}
	return "", "", "", "error: /env secret needs a name and a value.\n\n" + envHelp
}

// envHelp is /env help (and an empty list, and a mistake): what the agent's
// environment is for, and the formats with examples.
const envHelp = `**The agent's environment** is what its tasks get: every session has each entry as an
environment variable ($NAME). agent-runner's own settings are /config, never the agent's.

**A plain setting** — shown in /env, in the agent's prompt and in logs:
  /env set APP_ENV staging where the app deploys
  /env set GREETING "hello world"      (quote a value with spaces)

**A secret** — a credential: hidden in chat, logs and memory; the agent sees its name and description:
  /env secret GITEE_TOKEN 1a2b3c4d Gitee API and git for oam-server
  /env secret GITEE_SSH_KEY deploy key for oam      (a value of several lines on the lines after)
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...

**See it:** /env · **Remove one:** /env unset NAME
**A repo's login:** /repo add https://<user>@gitee.com/org/repo.git secret=GITEE_TOKEN

Names: upper case, digits and _; agent-runner's own (GIT_*, AGENT_*, ...) are refused. Setting a
name as the other kind moves it. A credential typed with /env set is refused, so it is never shown.
Changes apply from the next session. Send credentials only with /env secret: a token in an
ordinary message goes to the agent's model and its logs.`
