package execution

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/repos"
)

// The repos a session works on come from the registry (/repo,
// REPOS_DESIGN.md): checked out into its workspace from the fetched cache,
// pushed with each repo's own login.

func (h *Engine) repoRegistry() *repos.Registry { return repos.New(h.config.ReposFile) }

func (h *Engine) agentSecretStore() *agentsecrets.Store {
	return agentsecrets.New(h.config.SecretsFile)
}

// registeredRepos is the registry, or nothing (logged) if it is unreadable.
func (h *Engine) registeredRepos() []repos.Repo {
	if h.config.ReposFile == "" {
		return nil
	}
	list, err := h.repoRegistry().List()
	if err != nil {
		slog.Warn("repo registry unreadable", "error", err)
	}
	return list
}

func (h *Engine) repoNames() []string {
	var names []string
	for _, r := range h.registeredRepos() {
		names = append(names, r.Name)
	}
	return names
}

// checkoutRepos puts every registered repo into the workspace. A repo that
// cannot be fetched (a bad token, the host down) is a warning: the session
// runs without it.
func (h *Engine) checkoutRepos(sessionID string, live *agent.Session, checkoutPath string) {
	secrets := h.agentSecretStore()
	for _, rp := range h.registeredRepos() {
		if _, err := os.Lstat(filepath.Join(checkoutPath, rp.Name)); err == nil {
			continue // already there (a task's earlier turn checked it out)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err := repos.Checkout(ctx, rp, h.config.RepoCacheRoot, filepath.Join(checkoutPath, rp.Name), secrets)
		cancel()
		if err != nil {
			slog.Warn("repo checkout failed", "session_id", sessionID, "repo", rp.Name, "error", err)
			live.AddWarning("repo not available this session: " + err.Error())
			continue
		}
		slog.Info("workspace: checked out repo", "session_id", sessionID, "repo", rp.Name, "branch", rp.Branch)
	}
}

// repoOps maps each workspace repo to the git operations that push it: its
// registry entry's login. A repo missing from it is a workspace repo the
// registry does not know (the agent made or cloned it) or a read-only one.
func (h *Engine) repoOps(base *gitpkg.Operations, workspaceRepos []string) (ops map[string]*gitpkg.Operations, skipped []string) {
	byName := map[string]repos.Repo{}
	for _, r := range h.registeredRepos() {
		byName[r.Name] = r
	}
	secrets := h.agentSecretStore()
	ops = map[string]*gitpkg.Operations{}
	for _, path := range workspaceRepos {
		name := filepath.Base(path)
		rp, ok := byName[name]
		switch {
		case !ok:
			if hasCommits(path) {
				skipped = append(skipped, fmt.Sprintf("%s: not pushed: not a registered repo (ask the user to /repo add it)", name))
			}
		case !rp.Push:
			if hasUnpushedCommits(context.Background(), path) {
				skipped = append(skipped, fmt.Sprintf("%s: commits not pushed: the repo is read-only (push=false)", name))
			}
		default:
			o, err := rp.Ops(base, secrets)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s: not pushed: %v", name, err))
				continue
			}
			ops[path] = o
		}
	}
	return ops, skipped
}
