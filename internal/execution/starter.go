package execution

import (
	"fmt"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/clisetup"
)

// StartAgent validates config, creates an agent session, and starts the
// background loop. Together with GetAgentSession it makes the Engine satisfy
// botcommon.AgentStarter.
func (h *Engine) StartAgent(message, source string) (string, error) {
	// Fail fast on a missing CLI binary rather than burning workspace setup,
	// planning, and iteration retries on a session that can't run at all.
	if err := clisetup.PreflightAgentConfig(h.config.Agent.CLI); err != nil {
		return "", err
	}

	paths := h.config.Agent.Paths
	author := h.config.Agent.Author
	commitPrefix := h.config.Agent.CommitPrefix
	maxIter := h.config.Agent.MaxIterations
	maxSeconds := h.config.Agent.MaxTotalSeconds

	session, err := h.agentManager.CreateSession(
		message, paths,
		author, commitPrefix, maxIter, maxSeconds,
	)
	if err != nil {
		return "", err
	}
	session.Source = source

	// Missing credentials aren't fatal (some setups authenticate outside an
	// API key env var), but surface them immediately as a session warning
	// instead of letting the user wait for an opaque failure deep in the run.
	for _, w := range clisetup.BootstrapWarnings(clisetup.ResolveCLI(h.config.Agent.CLI), h.config.Agent.Provider) {
		session.AddWarning(w)
	}

	sessionID := session.ID
	if err := h.agentManager.Enqueue(session, h.ExecuteAgent); err != nil {
		h.FailSession(sessionID, "agent queue is full")
		return "", fmt.Errorf("agent queue is full")
	}

	return sessionID, nil
}

// GetAgentSession returns a snapshot of an agent session.
func (h *Engine) GetAgentSession(sessionID string) (*agent.Session, bool) {
	return h.agentManager.GetSession(sessionID)
}
