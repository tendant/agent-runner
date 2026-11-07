package execution

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/proposals"
	"github.com/agent-runner/agent-runner/internal/repos"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// An eval replay (agent.EvalSpec, PROACTIVE_DESIGN.md §11) runs a past task
// with a variant of the instructions and is judged by its checks; nothing
// of it is kept.

// evalCommandTimeout bounds one command check.
const evalCommandTimeout = 5 * time.Minute

// proposalTargetsFor is what a session may propose: nothing for a replay.
func proposalTargetsFor(h *Engine, eval *agent.EvalSpec) []proposalTarget {
	if eval != nil {
		return nil
	}
	return h.proposalTargets()
}

// evalRepos is the repos as a replay is told of them: read-only, so it is
// neither offered a self-push nor told its work will be pushed.
func evalRepos(list []repos.Repo, eval *agent.EvalSpec) []repos.Repo {
	if eval == nil {
		return list
	}
	out := make([]repos.Repo, len(list))
	for i, r := range list {
		r.Secret, r.Push = "", false
		out[i] = r
	}
	return out
}

// applyEvalSkills puts the variant's skills in the replay's workspace,
// unless the operator has a skill of that name (which wins in production
// too).
func (h *Engine) applyEvalSkills(sessionID, checkoutPath string, eval *agent.EvalSpec) {
	for target, content := range eval.Overrides {
		name := proposals.SkillName(target)
		if name == "" {
			continue
		}
		if h.config.Agent.SkillsDir != "" {
			if _, err := os.Stat(filepath.Join(h.config.Agent.SkillsDir, name)); err == nil {
				continue
			}
		}
		for _, d := range []string{".claude", ".agents"} {
			dir := filepath.Join(checkoutPath, d, "skills", name)
			err := os.MkdirAll(dir, 0o755)
			if err == nil {
				err = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
			}
			if err != nil {
				slog.Warn("eval: could not put the variant's skill in the workspace", "session_id", sessionID, "skill", name, "error", err)
			}
		}
	}
}

// withFile is r with the memory file filename's content replaced (or
// added): an eval replay's variant of GOALS.md.
func withFile(r tmpl.Retrieval, filename, content string) tmpl.Retrieval {
	out := tmpl.Retrieval{Files: append([]tmpl.MemoryFile(nil), r.Files...)}
	content = strings.TrimSpace(content)
	for i, f := range out.Files {
		if f.Filename == filename {
			out.Files[i].Content = content
			return out
		}
	}
	if content != "" {
		out.Files = append(out.Files, tmpl.MemoryFile{Name: strings.TrimSuffix(filename, ".md"), Filename: filename, Content: content})
	}
	return out
}

// runEvalChecks judges a replay by its checks and records the results on
// the session. Command checks run in the replay's own sandbox (the run's
// launcher on ctx), never on the host: without a sandbox they fail, unrun.
func (h *Engine) runEvalChecks(ctx context.Context, sessionID string, live *agent.Session, checkoutPath string, eval *agent.EvalSpec) {
	snap := live.Snapshot()
	var reply strings.Builder
	for _, it := range snap.Iterations {
		reply.WriteString(it.Output)
		reply.WriteString("\n")
	}
	results := make([]agent.EvalResult, 0, len(eval.Checks))
	for _, c := range eval.Checks {
		r := agent.EvalResult{Check: c}
		switch c.Kind {
		case "file":
			rel := filepath.Clean(filepath.FromSlash(c.Arg))
			if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				r.Detail = "not a path in the workspace"
				break
			}
			if _, err := os.Lstat(filepath.Join(checkoutPath, rel)); err == nil {
				r.Pass = true
			} else {
				r.Detail = "missing"
			}
		case "reply":
			r.Pass = strings.Contains(strings.ToLower(reply.String()), strings.ToLower(c.Arg))
			if !r.Pass {
				r.Detail = "not in the reply"
			}
		case "command":
			r.Pass, r.Detail = h.evalCommand(ctx, checkoutPath, c.Arg)
		default:
			r.Detail = "unknown check " + c.Kind
		}
		results = append(results, r)
	}
	live.SetEvalResults(results)
	pass := 0
	for _, r := range results {
		if r.Pass {
			pass++
		}
	}
	slog.Info("eval: checks", "session_id", sessionID, "passed", pass, "of", len(results))
}

func (h *Engine) evalCommand(ctx context.Context, dir, command string) (bool, string) {
	if !h.sandboxed() {
		return false, "not run: command checks need a sandbox (AGENT_SANDBOX)"
	}
	l, ok := executor.LauncherFrom(ctx)
	if !ok {
		return false, "not run: no sandbox for this run"
	}
	cctx, cancel := context.WithTimeout(ctx, evalCommandTimeout)
	defer cancel()
	cmd, release, err := l.Command(cctx, executor.LaunchSpec{Name: "sh", Args: []string{"-c", command}, Dir: dir})
	if err != nil {
		return false, "not run: " + err.Error()
	}
	defer release()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	tail := strings.TrimSpace(out.String())
	if len(tail) > 300 {
		tail = "…" + tail[len(tail)-300:]
	}
	if err != nil {
		return false, fmt.Sprintf("%v: %s", err, tail)
	}
	return true, ""
}
