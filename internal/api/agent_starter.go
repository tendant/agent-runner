package api

import (
	"fmt"

	"github.com/agent-runner/agent-runner/internal/agent"
)

// AgentStarter is the interface used by the Telegram bot to start and poll agent sessions.
type AgentStarter interface {
	StartAgent(message string) (sessionID string, err error)
	GetAgentSession(sessionID string) (*agent.Session, bool)
}

// AgentStarterAdapter bridges the Telegram bot to the existing agent creation logic.
type AgentStarterAdapter struct {
	handlers *Handlers
}

// NewAgentStarterAdapter creates an adapter that delegates to Handlers.
func NewAgentStarterAdapter(h *Handlers) *AgentStarterAdapter {
	return &AgentStarterAdapter{handlers: h}
}

// StartAgent validates config, creates an agent session, and starts the background loop.
// It parses @project-name from the message if present, falling back to DefaultProject.
func (a *AgentStarterAdapter) StartAgent(message string) (string, error) {
	h := a.handlers

	// Parse @project tag from message
	tagProject, cleanMessage := parseProjectTag(message)
	project := tagProject
	if project != "" {
		message = cleanMessage
	}
	if project == "" {
		project = h.config.Agent.DefaultProject
	}

	paths := h.config.Agent.Paths
	if len(paths) == 0 {
		return "", fmt.Errorf("agent not configured: AGENT_PATHS is required")
	}

	if project != "" && !h.config.IsProjectAllowed(project) {
		return "", fmt.Errorf("project not in allowed_projects")
	}

	author := h.config.Agent.Author
	commitPrefix := h.config.Agent.CommitPrefix
	maxIter := h.config.Agent.MaxIterations
	maxSeconds := h.config.Agent.MaxTotalSeconds

	session, err := h.agentManager.CreateSession(
		project, message, paths,
		author, commitPrefix, maxIter, maxSeconds,
	)
	if err != nil {
		return "", err
	}

	sessionID := session.ID
	go h.executeAgent(session)

	return sessionID, nil
}

// GetAgentSession returns a snapshot of an agent session.
func (a *AgentStarterAdapter) GetAgentSession(sessionID string) (*agent.Session, bool) {
	return a.handlers.agentManager.GetSession(sessionID)
}
