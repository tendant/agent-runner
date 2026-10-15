package execution

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/metrics"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// maxConflictResolutions is how many times the agent gets to resolve rebase
// conflicts before the runner parks the work on a rescue branch instead.
const maxConflictResolutions = 2

// syncWorkspaceRepos pushes every repo the agent committed to. Sessions run
// in parallel without locks, so a push is routinely rejected because
// another session moved the branch; git.Push rebases and retries on its
// own, and when the rebase conflicts the agent is given an iteration to
// resolve it the way a person would. Work that still can't be merged is
// pushed to agent-rescue/<session> so deleting the workspace afterwards
// loses nothing. Non-fatal: every outcome short of "pushed" is a warning.
func (h *Engine) syncWorkspaceRepos(
	ctx context.Context, sessionID, source string, liveSession *agent.Session,
	checkoutPath, message string, deadline time.Time,
	plan *subagent.PlanResult, promptBuilder *subagent.PromptBuilder, as *agentSession,
) {
	if len(workspaceRepos(checkoutPath)) == 0 {
		return
	}
	base := gitpkg.NewOperations(h.config.GitPushRetries, h.config.GitPushRetryDelaySeconds)

	for attempt := 0; ; attempt++ {
		// Sandboxed, nothing of the agent's may still be running when the
		// runner runs git on the host: something it left behind could
		// rewrite .git/config right after the saved one is put back.
		// Closing its session ends its sandbox and every process in it.
		if h.sandboxed() && as != nil {
			as.Close()
		}
		// Every round: the agent may have run since the last one.
		candidates, untrusted := h.pushableRepos(checkoutPath)
		ops, skipped := h.repoOps(base, candidates)
		if attempt == 0 {
			for _, r := range untrusted {
				if !hasCommits(r) {
					continue
				}
				liveSession.AddWarning(fmt.Sprintf("%s: not pushed: not a repo agent-runner set up (ask the user to /repo add it)", filepath.Base(r)))
			}
			for _, w := range skipped {
				liveSession.AddWarning(w)
			}
		}
		conflicts, warnings := pushRepos(ctx, ops, candidates)
		if len(conflicts) == 0 {
			for _, w := range warnings {
				liveSession.AddWarning(w)
			}
			return
		}

		canResolve := as != nil && promptBuilder != nil && attempt < maxConflictResolutions &&
			!liveSession.StopRequested() && ctx.Err() == nil && time.Now().Before(deadline)
		if !canResolve {
			for _, w := range warnings {
				liveSession.AddWarning(w)
			}
			for _, rc := range conflicts {
				liveSession.AddWarning(parkConflictedWork(ctx, ops[rc.RepoPath], rc, sessionID))
			}
			return
		}

		// Hand the conflict to the agent as a corrective iteration, the same
		// way reviewer feedback is.
		conflictContext := buildConflictContext(conflicts)
		iterNum := liveSession.Snapshot().CurrentIteration + 1
		var systemPrompt string
		if h.config.Agent.PlannerEnabled {
			systemPrompt = promptBuilder.Build(ctx, checkoutPath, plan, iterNum, message, conflictContext)
		} else {
			systemPrompt = promptBuilder.BuildStatic(message, conflictContext)
		}
		slog.Info("running conflict-resolution iteration", "session_id", sessionID,
			"iteration", iterNum, "repos", len(conflicts), "attempt", attempt+1)
		liveSession.AppendExecEvent(string(executor.EventWarning),
			fmt.Sprintf("remote moved; resolving rebase conflicts in %s", conflictSummary(conflicts)), time.Now())

		iterCtx, iterSpan := tracing.Start(ctx, "agent.iteration",
			attribute.Int("iteration.number", iterNum),
			attribute.Bool("iteration.retry", true),
			attribute.String("iteration.reason", "rebase_conflict"),
		)
		if as.toolSpans != nil {
			as.toolSpans.SetParent(iterCtx)
		}
		if h.sandboxed() {
			// Closed above, before the runner's git: start it again.
			if err := as.start(ctx, checkoutPath); err != nil {
				iterSpan.End()
				for _, rc := range conflicts {
					liveSession.AddWarning(parkConflictedWork(ctx, ops[rc.RepoPath], rc, sessionID))
				}
				liveSession.AddWarning("could not restart the agent to resolve conflicts: " + err.Error())
				return
			}
			as.forceFull = true
		}
		result, _ := h.executePrompt(iterCtx, as.session(), executor.PromptRequest{SystemPrompt: systemPrompt, Message: message}, iterNum, deadline, liveSession)
		result.Prompt = systemPrompt
		result.Retry = true
		liveSession.AddIteration(result)
		iterSpan.SetAttributes(attribute.String("iteration.status", string(result.Status)), attribute.Float64("iteration.cost_usd", result.CostUSD))
		if result.Error != "" {
			tracing.Fail(iterSpan, result.Error)
		}
		iterSpan.End()
		metrics.IterationsTotal.WithLabelValues(string(result.Status), source).Inc()
		metrics.IterationDurationSeconds.WithLabelValues(source).Observe(float64(result.DurationSecs))
		if result.CostUSD > 0 {
			metrics.CostUSDTotal.WithLabelValues(source).Add(result.CostUSD)
		}
	}
}

// pushableRepos is the repos the runner may run git in on the host: all of
// them unsandboxed; sandboxed, those it set up, with their saved configs
// put back.
func (h *Engine) pushableRepos(checkoutPath string) (repos, untrusted []string) {
	if !h.sandboxed() {
		return workspaceRepos(checkoutPath), nil
	}
	return restoreGitConfigs(checkoutPath)
}

// pushRepos pushes each repo with unpushed commits, with its own login
// (ops; a repo with none is not pushed). Repos whose rebase hit conflicts
// come back in conflicts (rebase left in progress); any other push failure
// becomes a warning string.
func pushRepos(ctx context.Context, opsFor map[string]*gitpkg.Operations, repos []string) (conflicts []*gitpkg.RebaseConflictError, warnings []string) {
	for _, repo := range repos {
		ops, ok := opsFor[repo]
		if !ok {
			continue
		}
		name := filepath.Base(repo)
		if ops.RebaseInProgress(ctx, repo) {
			// The agent resolved the markers but didn't finish, or didn't
			// touch it at all. Try to finish; what's left conflicted is the
			// agent's to resolve.
			if files := ops.ConflictedFiles(ctx, repo); len(files) > 0 {
				branch := rebaseBranch(ctx, ops, repo)
				conflicts = append(conflicts, &gitpkg.RebaseConflictError{RepoPath: repo, Branch: branch, Files: files})
				continue
			}
			if err := ops.ContinueRebase(ctx, repo); err != nil {
				if files := ops.ConflictedFiles(ctx, repo); len(files) > 0 {
					conflicts = append(conflicts, &gitpkg.RebaseConflictError{RepoPath: repo, Branch: rebaseBranch(ctx, ops, repo), Files: files, Err: err})
					continue
				}
				warnings = append(warnings, fmt.Sprintf("%s: could not finish in-progress rebase: %v", name, err))
				continue
			}
		}
		if !hasUnpushedCommits(ctx, repo) {
			continue
		}
		err := ops.Push(ctx, repo)
		if err == nil {
			continue
		}
		var rc *gitpkg.RebaseConflictError
		if errors.As(err, &rc) {
			conflicts = append(conflicts, rc)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("agent committed in %s but git push failed: %v", name, err))
	}
	return conflicts, warnings
}

// parkConflictedWork abandons the rebase and pushes the session's local
// commits to agent-rescue/<session> so they survive workspace cleanup.
func parkConflictedWork(ctx context.Context, ops *gitpkg.Operations, rc *gitpkg.RebaseConflictError, sessionID string) string {
	name := filepath.Base(rc.RepoPath)
	if err := ops.AbortRebase(ctx, rc.RepoPath); err != nil {
		slog.Warn("git: rebase --abort failed while parking work", "repo", rc.RepoPath, "error", err)
	}
	rescue := "agent-rescue/" + sessionID
	if err := ops.PushToBranch(ctx, rc.RepoPath, rescue); err != nil {
		return fmt.Sprintf("%s: unresolved rebase conflict in %s and the local commits could not be preserved: %v",
			name, strings.Join(rc.Files, ", "), err)
	}
	slog.Warn("git: parked unmerged work on rescue branch", "repo", rc.RepoPath, "branch", rescue, "files", rc.Files)
	return fmt.Sprintf("%s: unresolved rebase conflict in %s — local commits pushed to branch %s for manual merge",
		name, strings.Join(rc.Files, ", "), rescue)
}

// buildConflictContext is the prompt section that tells the agent exactly
// what state the repo is in and how to finish: it finishes the rebase, and
// the runner pushes.
func buildConflictContext(conflicts []*gitpkg.RebaseConflictError) string {
	var sb strings.Builder
	sb.WriteString("## Git: the remote branch moved — resolve the rebase conflicts\n\n")
	sb.WriteString("Another session pushed while you worked. Your commits were rebased onto the new remote head and the rebase stopped on conflicts. It is still in progress; finish it rather than redoing the task.\n\n")
	for _, rc := range conflicts {
		fmt.Fprintf(&sb, "**Repo:** `%s` (branch `%s`)\n**Conflicted files:**\n", rc.RepoPath, rc.Branch)
		for _, f := range rc.Files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("For each repo above, in that directory:\n")
	sb.WriteString("1. Open each conflicted file and resolve the `<<<<<<<`/`=======`/`>>>>>>>` markers. Keep both sides' intent — the remote changes come from a different task and must be preserved.\n")
	sb.WriteString("2. `git add <files>` then `GIT_EDITOR=true git rebase --continue`. Repeat if a later commit conflicts.\n")
	sb.WriteString("3. Stop there: the runner pushes once the rebase is finished.\n\n")
	sb.WriteString("Do not `git rebase --abort`, do not force-push, do not reset the branch.\n")
	return sb.String()
}

func conflictSummary(conflicts []*gitpkg.RebaseConflictError) string {
	parts := make([]string, 0, len(conflicts))
	for _, rc := range conflicts {
		parts = append(parts, fmt.Sprintf("%s (%d files)", filepath.Base(rc.RepoPath), len(rc.Files)))
	}
	return strings.Join(parts, ", ")
}

// rebaseBranch names the branch a rebase in progress will land on.
func rebaseBranch(ctx context.Context, ops *gitpkg.Operations, repo string) string {
	if b, err := ops.RebaseHeadName(ctx, repo); err == nil && b != "" {
		return b
	}
	return "HEAD"
}

// workspaceRepos returns the git repos the agent may have committed to:
// the checkout itself if it is one, otherwise its immediate subdirectories
// that are (the shared-repo layout PrepareAgentWorkspace builds).
func workspaceRepos(checkoutPath string) []string {
	if checkoutPath == "" {
		return nil
	}
	if isGitRepo(checkoutPath) {
		return []string{checkoutPath}
	}
	entries, err := os.ReadDir(checkoutPath)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		p := filepath.Join(checkoutPath, e.Name())
		if isGitRepo(p) {
			repos = append(repos, p)
		}
	}
	sort.Strings(repos)
	return repos
}

func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// hasUnpushedCommits reports commits ahead of the upstream tracking branch.
// No upstream means nothing to push.
// hasCommits reports a repo with at least one commit: one the agent made
// and left empty is not worth a warning. It reads files, never runs git:
// the repo may be one agent-runner did not set up, whose config it does
// not trust.
func hasCommits(repoPath string) bool {
	gitDir := filepath.Join(repoPath, ".git")
	if fi, err := os.Stat(filepath.Join(gitDir, "packed-refs")); err == nil && fi.Size() > 0 {
		return true
	}
	found := false
	filepath.WalkDir(filepath.Join(gitDir, "refs", "heads"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func hasUnpushedCommits(ctx context.Context, repoPath string) bool {
	out, err := gitCmd(ctx, repoPath, "log", "@{u}..HEAD", "--oneline").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}
