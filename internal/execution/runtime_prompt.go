package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

// The runtime layer of a session's prompt (MEMORY_DESIGN.md §3): how this
// runner works, generated per session from the code that does the work, so
// it never goes stale the way a seeded agent.md does. agent.md keeps only
// what the operator knows (persona, house rules).

// runtimeFacts is what the runtime layer depends on.
type runtimeFacts struct {
	Sandboxed  bool
	MemoryDir  string                // as the agent sees it
	MemoryCopy bool                  // MemoryDir is a staged copy, merged back at the end
	Secrets    []agentsecrets.Secret // the agent user's secrets: names and descriptions are shown
	Repos      []repos.Repo          // the registered repos in the workspace
}

func runtimeInstructions(f runtimeFacts) string {
	var b strings.Builder
	b.WriteString("\n\n## How this runner works\n\n")
	b.WriteString("Written by agent-runner for this session. Where anything above disagrees, this wins.\n")

	b.WriteString("\n### Git\n\n")
	if len(f.Repos) > 0 {
		b.WriteString("The repos in your working directory, set up by the people you work for (`/repo`):\n\n")
		for _, r := range f.Repos {
			branch := r.Branch
			if branch == "" {
				branch = "its default branch"
			}
			push := "pushed for you"
			if !r.Push {
				push = "read-only: never pushed"
			}
			b.WriteString("- `" + r.Name + "/`: " + r.URL + ", on " + branch + ", " + push + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Commit your work with descriptive messages, but do not `git push`, `git pull` or change remotes:\n" +
		"when the session ends, agent-runner pushes your commits to the branch you are on. If another session\n" +
		"pushed meanwhile, it rebases them for you, and asks you to resolve the conflicts only if that rebase\n" +
		"conflicts. Repos you clone or create yourself are not pushed: to work on another repo, ask the user\n" +
		"to add it with `/repo add`.\n")

	if f.MemoryDir != "" {
		dir, _ := filepath.Abs(f.MemoryDir)
		b.WriteString("\n### Memory\n\n")
		b.WriteString("Your memory directory is " + dir + " (an absolute path: write files directly in it, never in a\n" +
			"`memory` subdirectory). Its files are loaded into every session, and several people talk to\n" +
			"this agent and share them: save what will help future sessions, briefly, and never secrets.\n\n" +
			"- `user_preferences.md`: how the people you work for like things done\n" +
			"- `project_summary.md`: goals, tech stack, status\n" +
			"- `decisions.md`: decisions and why\n" +
			"- `lessons.md`: lessons learned; append only\n" +
			"- any other lowercase `.md` file, for a topic of its own (e.g. `oam_server.md`): these are only\n" +
			"  listed in your prompt (Memory Files), by their first line, so start each with a one-line\n" +
			"  summary, and read one when the task touches its topic\n\n" +
			"Append; delete only to correct an error. `agent.md` and `prompt.md` are your instructions, set by\n" +
			"the operator: do not edit them; propose changes in your reply instead.\n")
		if f.MemoryCopy {
			b.WriteString("\nThis directory is your own copy. Changes to its lowercase `.md` files (new ones too) are\n" +
				"saved when the session ends; `agent.md`, `prompt.md`, upper-case files such as `HEARTBEAT.md`,\n" +
				"daily logs, deletions, subdirectories and links are not.\n")
		}
	}

	b.WriteString("\n### Credentials\n\n")
	b.WriteString("Credentials for your tasks are environment variables: use them from there, never search files\n" +
		"for keys, and do not write them into files, commits or memory. agent-runner's own credentials\n" +
		"(model keys) are never yours to use: it calls the model for you. If a task needs a credential you\n" +
		"do not have, ask the user to add it in chat with `/secret set NAME VALUE description` (e.g.\n" +
		"`/secret set GITEE_TOKEN <token> Gitee API`; `/secret help` shows the format) and to start a new\n" +
		"session; never ask them to paste it into a message, which reaches your model and the logs.")
	b.WriteString("\n")
	if len(f.Secrets) > 0 {
		b.WriteString("\nSet for you (`/secret` in chat adds more):\n\n")
		for _, s := range f.Secrets {
			line := "- `$" + s.Name + "`"
			if s.Description != "" {
				line += ": " + s.Description
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n### Files for the user\n\n")
	b.WriteString("To send the user files, put them in `_send/` in your working directory: they are delivered when\n" +
		"the session ends.\n")
	return b.String()
}

// PreviewPrompt is the prompt a session for message would get now, as
// /prompt preview shows it. A sandboxed session's memory copy is shown at
// the memory dir's own path.
func (h *Engine) PreviewPrompt(message string) (string, error) {
	p, err := h.resolvePrompt("preview", message, h.config.MemoryDir)
	if err != nil {
		return "", err
	}
	secrets, _ := agentsecrets.New(h.config.SecretsFile).List()
	return p + runtimeInstructions(runtimeFacts{Sandboxed: h.sandboxed(), MemoryDir: h.config.MemoryDir, MemoryCopy: true, Secrets: secrets, Repos: h.registeredRepos()}), nil
}

// threadKey names the thread a session belongs to, for its daily-log entry
// and to show a later session of the same thread its entries first: the
// task, else the chat (hashed: the log is pushed with memory), else none.
func threadKey(s *agent.Session) string {
	if s == nil {
		return ""
	}
	if s.TaskWorkspace != "" {
		return "task-" + filepath.Base(s.TaskWorkspace)
	}
	if s.ConvID != "" {
		h := sha256.Sum256([]byte(s.ConvID))
		return "chat-" + hex.EncodeToString(h[:4])
	}
	return ""
}

// agentSecrets lists the agent user's secrets for the session's prompt. A
// broken secrets file is reported, not fatal: the session runs without them.
func (h *Engine) agentSecrets(sessionID string, live *agent.Session) []agentsecrets.Secret {
	if h.config.SecretsFile == "" {
		return nil
	}
	list, err := agentsecrets.New(h.config.SecretsFile).List()
	if err != nil {
		slog.Warn("agent secrets unreadable; the session runs without them", "session_id", sessionID, "error", err)
		live.AddWarning("agent secrets not loaded: " + err.Error())
		return nil
	}
	return list
}
