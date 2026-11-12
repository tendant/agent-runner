package chatcmd

import (
	"fmt"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

// agent-runner's own secrets (/config secret, config.RunnerSecretsFile):
// credentials the runner uses for the agent and never hands it, such as the
// ones the sandbox's egress gateway adds to requests (AGENT_SANDBOX_INJECT).
// The agent's secrets are /env secret. Replies never repeat a value.

func (c *Commander) runnerSecrets() (*agentsecrets.Store, string) {
	if c.cfg.RunnerSecretsFile == "" {
		return nil, "error: agent-runner has no state directory (STATE_ROOT) to keep its secrets in"
	}
	return agentsecrets.New(c.cfg.RunnerSecretsFile), ""
}

// handleConfigSecret is /config secret NAME VALUE [description], or the
// value on the lines after "NAME [description]".
func (c *Commander) handleConfigSecret(arg string) string {
	store, errText := c.runnerSecrets()
	if errText != "" {
		return errText
	}
	name, value, desc, errText := parseSecret(arg, strings.Fields(arg))
	if errText != "" {
		return "error: /config secret needs a name and a value.\n\n" + configSecretHelp
	}
	if strings.Contains(value, sealedMask) {
		return errMaskedValue
	}
	if err := agentsecrets.CheckNameFor(name, c.cfg.Agent.CLI); err != nil {
		return "error: " + err.Error()
	}
	if err := store.Set(name, value, desc); err != nil {
		return "error: " + err.Error()
	}
	note := ""
	if _, agents, err := agentsecrets.Load(c.cfg.SecretsFile, ""); err == nil {
		for _, s := range agents {
			if s.Name == name {
				note = " The agent also has an /env secret of that name, its own: /env unset " + name + " if it should not."
			}
		}
	}
	return fmt.Sprintf("agent-runner's secret: %s (%d characters, hidden), never the agent's; AGENT_SANDBOX_INJECT rules use it as $%s from the next session.%s",
		name, len(value), name, note)
}

// handleConfigSecrets is /config secrets: agent-runner's secrets, by name.
func (c *Commander) handleConfigSecrets() string {
	store, errText := c.runnerSecrets()
	if errText != "" {
		return errText
	}
	list, err := store.List()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(list) == 0 {
		return "agent-runner has no secrets of its own.\n\n" + configSecretHelp
	}
	var b strings.Builder
	b.WriteString("agent-runner's secrets (never the agent's):\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s=%s", s.Name, sealedMask)
		if s.Description != "" {
			fmt.Fprintf(&b, ": %s", s.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// unsetRunnerSecret removes one of agent-runner's secrets, reporting
// whether there was one.
func (c *Commander) unsetRunnerSecret(name string) (bool, error) {
	if c.cfg.RunnerSecretsFile == "" {
		return false, nil
	}
	return agentsecrets.New(c.cfg.RunnerSecretsFile).Remove(name)
}

const configSecretHelp = `**agent-runner's secrets** are credentials it uses for the agent and never hands it: the
sandbox's egress gateway adds them to the agent's HTTPS requests (AGENT_SANDBOX_INJECT), so the
agent can call an API without holding its token. The agent's own secrets are /env secret.

  /config secret GITHUB_API_TOKEN ghp_... GitHub API, added for the agent
  /config set AGENT_SANDBOX_INJECT "api.github.com:Authorization=Bearer $GITHUB_API_TOKEN"

**See them:** /config secrets (names only) · **Remove one:** /config unset NAME`
