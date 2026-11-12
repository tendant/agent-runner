package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/chatlog"
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
	Agenda     bool                  // proactive checks are on: keep agenda.md (PROACTIVE_DESIGN.md)
	Secrets    []agentsecrets.Secret // the agent's secrets: names and descriptions are shown
	Env        []agentsecrets.Secret // the agent's plain settings: shown with their values
	Repos      []repos.Repo          // the registered repos in the workspace
	ChatThread string                // the session's thread in _chats/ ("": no chat record)
	Proposals  []proposalTarget      // what may be proposed (_proposals/); nil: proposals are off
	Services   []string              // the network services the session may reach (AGENT_SANDBOX_SERVICES)
	Injected   []sandbox.InjectGrant // credentials the sandbox's gateway adds to requests (AGENT_SANDBOX_INJECT)
}

func runtimeInstructions(f runtimeFacts) string {
	var b strings.Builder
	b.WriteString("\n\n## How this runner works\n\n")
	b.WriteString("Written by agent-runner for this session. Where anything above disagrees, this wins.\n")

	b.WriteString("\n### Git\n\n")
	var selfPush []repos.Repo
	if len(f.Repos) > 0 {
		b.WriteString("The repos in your working directory, set up by the people you work for (`/repo`):\n\n")
		for _, r := range f.Repos {
			branch := r.Branch
			if branch == "" {
				branch = "its default branch"
			}
			push := "pushed for you"
			switch {
			case !r.Push:
				push = "read-only: never pushed"
			case r.Secret != "":
				push = "pushed for you; you may also push it yourself (below)"
				selfPush = append(selfPush, r)
			}
			b.WriteString("- `" + r.Name + "/`: " + r.URL + ", on " + branch + ", " + push + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Commit your work with descriptive messages, but do not `git pull`, change remotes or git config:\n" +
		"when the session ends, agent-runner pushes your commits to the branch you are on. If another session\n" +
		"pushed meanwhile, it rebases them for you, and asks you to resolve the conflicts only if that rebase\n" +
		"conflicts. Repos you clone or create yourself are not pushed: to work on another repo, ask the user\n" +
		"to add it with `/repo add`.\n")
	if len(selfPush) > 0 {
		b.WriteString("\nWhen your commits need to be on the remote before the session ends (to trigger CI or a deploy\n" +
			"and check the result), push them yourself, to the branch you are on, with the repo's command below;\n" +
			"its login is the repo's secret, already in your environment. Never force-push or push to another\n" +
			"branch. If the push is rejected because the remote moved, leave it: agent-runner rebases and pushes\n" +
			"at the end, and you can check the result in the next turn.\n\n")
		for _, r := range selfPush {
			b.WriteString("- `" + r.Name + "/`: `" + selfPushCommand(r) + "`\n")
		}
		b.WriteString("\nOtherwise, do not `git push`.\n")
	} else {
		b.WriteString("Leave the pushing to agent-runner: do not `git push`.\n")
	}

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
			"  summary, and read one when the task touches its topic\n")
		if f.Agenda {
			b.WriteString("- `agenda.md`: your open loops, one checklist line each, e.g.\n" +
				"  `- [ ] check the v0.1.4 deploy is healthy · due: 2026-11-08 18:00 · id: deploy-v014`.\n" +
				"  Add one when you leave something unfinished or promise to look again later; mark it\n" +
				"  `- [x]` when done. agent-runner checks due items and raises them with the people you work\n" +
				"  for. `GOALS.md`, if present, holds the operator's standing goals: read it, never edit it\n")
		}
		b.WriteString("\n" +
			"Append; delete only to correct an error. `agent.md` and `prompt.md` are your instructions, set by\n" +
			"the operator: do not edit them; ")
		if len(f.Proposals) > 0 {
			b.WriteString("propose changes to them in `_proposals/` (below).\n")
		} else {
			b.WriteString("propose changes in your reply instead.\n")
		}
		if f.MemoryCopy {
			b.WriteString("\nThis directory is your own copy. Changes to its lowercase `.md` files (new ones too) are\n" +
				"saved when the session ends; `agent.md`, `prompt.md`, upper-case files such as `HEARTBEAT.md`,\n" +
				"daily logs, deletions, subdirectories and links are not.\n")
		}
	}

	b.WriteString("\n### Your environment\n\n")
	b.WriteString("What your tasks need is in your environment, set by the people you work for with `/env` in chat:\n" +
		"use it from there, never search files for keys, and do not write credentials into files, commits\n" +
		"or memory. agent-runner's own settings and keys (`/config`) are never yours to use: it calls the\n" +
		"model for you. If a task needs something you do not have, ask the user to add it in chat:\n" +
		"`/env secret NAME VALUE description` for a credential (e.g. `/env secret GITEE_TOKEN <token> Gitee API`),\n" +
		"`/env set NAME VALUE description` for a plain setting (`/env help` shows the format), and to start\n" +
		"a new session; never ask them to paste a credential into a message, which reaches your model and\n" +
		"the logs.\n")
	if len(f.Env) > 0 {
		b.WriteString("\nSettings:\n\n")
		for _, v := range f.Env {
			line := "- `" + v.Name + "=" + v.Value + "`"
			if v.Description != "" {
				line += ": " + v.Description
			}
			b.WriteString(line + "\n")
		}
	}
	if len(f.Secrets) > 0 {
		b.WriteString("\nSecrets (their values are hidden from what you write):\n\n")
		for _, s := range f.Secrets {
			line := "- `$" + s.Name + "`"
			if s.Description != "" {
				line += ": " + s.Description
			}
			b.WriteString(line + "\n")
		}
	}

	if len(f.Injected) > 0 {
		b.WriteString("\nSigned in for you (agent-runner adds the credential to each HTTPS request to the host, so it is not\n" +
			"in your environment; send these requests without one, and use https):\n\n")
		for _, g := range f.Injected {
			b.WriteString("- `" + g.Host + "`: " + g.Header + " from `$" + strings.Join(g.Credentials, "`, `$") + "`\n")
		}
	}

	if len(f.Services) > 0 {
		b.WriteString("\n### Services\n\n")
		b.WriteString("Network services set up for you (a database, a cache, an internal API). Connect to each at the\n" +
			"address in your environment, not at a hostname from code or docs: from where you run, that is how\n" +
			"it is reached. A login it needs is among your secrets.\n\n")
		for _, name := range f.Services {
			k := "$AGENT_SERVICE_" + strings.ToUpper(name)
			b.WriteString("- `" + name + "`: `" + k + "_ADDR` (host:port; also `" + k + "_HOST`, `" + k + "_PORT`)\n")
		}
	}

	if f.ChatThread != "" {
		b.WriteString("\n### Earlier chats\n\n")
		b.WriteString("`" + chatlog.DirName + "/` in your working directory holds what was said in this chat over the last\n" +
			"few months, word for word: a folder per conversation thread, a file per day\n" +
			"(`" + chatlog.DirName + "/<thread>/YYYY-MM-DD.md`). This conversation is `" + chatlog.DirName + "/" + f.ChatThread + "/`.\n" +
			"The prompt carries only its latest messages: when the user refers to something said earlier, or\n" +
			"asks about a past conversation, search there (e.g. `grep -ri`) before saying you don't know.\n" +
			"It is a copy made for this session: changing it changes nothing.\n")
	}

	if len(f.Proposals) > 0 {
		b.WriteString("\n### Proposing changes to your instructions\n\n")
		b.WriteString("When you see a pattern your instructions should cover (the same mistake in several sessions, a\n" +
			"procedure people keep explaining), propose a change: write `_proposals/<short-name>.md` in your\n" +
			"working directory. A person reviews each one; nothing changes until they approve it. Propose only\n" +
			"what the evidence supports, at most 3 per session, and never what lessons.md says was rejected.\n\n" +
			"```\n---\ntarget: prompt.md\nbase: <the target's base below; omit for a new skill>\n" +
			"title: <one line>\nevidence: [<session ids or dates>]\n---\n\n## Why\n<what happened, and why this change helps>\n\n" +
			"## Change\n<the full new content of the target: not a diff>\n```\n\n" +
			"What you may propose, with its current base:\n\n")
		for _, t := range f.Proposals {
			base := "new"
			if t.Base != "" {
				base = t.Base
			}
			b.WriteString("- `" + t.Target + "`: " + base + "\n")
		}
		b.WriteString("- `skills/<name>/SKILL.md`: a new skill of your own (lowercase name), in every later session's\n" +
			"  workspace once approved\n")
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
	plain, secrets, _ := agentsecrets.Load(h.config.SecretsFile, h.config.EnvFile)
	return p + runtimeInstructions(runtimeFacts{Sandboxed: h.sandboxed(), MemoryDir: h.config.MemoryDir, MemoryCopy: true, Agenda: h.config.Agent.Proactive, Secrets: secrets, Env: plain, Repos: h.registeredRepos(), Proposals: h.proposalTargets()}), nil
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

// agentEnv loads the agent's environment for a session: its plain settings
// and its secrets. A broken file is reported, not fatal: the session runs
// without them.
func (h *Engine) agentEnv(sessionID string, live *agent.Session) (plain, secrets []agentsecrets.Secret) {
	if h.config.SecretsFile == "" {
		return nil, nil
	}
	plain, secrets, err := agentsecrets.Load(h.config.SecretsFile, h.config.EnvFile)
	if err != nil {
		slog.Warn("agent environment unreadable; the session runs without it", "session_id", sessionID, "error", err)
		live.AddWarning("agent environment (/env) not loaded: " + err.Error())
		return nil, nil
	}
	return plain, secrets
}

// selfPushCommand is the command the agent pushes repo r with: the login is
// r's secret from the agent's environment, given for that one command (a
// credential helper on the command line, or an ssh key in a temporary file),
// so nothing is written to git config, where the runner's host-side git
// would read it.
func selfPushCommand(r repos.Repo) string {
	if r.IsSSH() {
		return `k=$(mktemp) && printf '%s\n' "$` + r.Secret + `" > "$k" && chmod 600 "$k" && GIT_SSH_COMMAND="ssh -i $k -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new" git -C ` + r.Name + ` push origin HEAD; rm -f "$k"`
	}
	return `git -C ` + r.Name + ` -c credential.helper= -c credential.helper='!f() { echo username=` + r.User() + `; echo "password=$` + r.Secret + `"; }; f' push origin HEAD`
}

// stageChats puts a copy of the session's chat record (internal/chatlog)
// in _chats/ of its working directory, replacing an earlier turn's, and
// returns the session's own thread folder there; "" when the session has no
// chat or nothing was recorded.
func (h *Engine) stageChats(sessionID string, s *agent.Session, checkoutPath string) string {
	dst := filepath.Join(checkoutPath, chatlog.DirName)
	if s.ConvID == "" || h.config.ChatsRoot == "" {
		_ = os.RemoveAll(dst)
		return ""
	}
	thread, err := chatlog.CopyChat(h.config.ChatsRoot, s.ConvID, dst)
	if err != nil {
		slog.Warn("could not copy the chat record into the workspace", "session_id", sessionID, "error", err)
		s.AddWarning("earlier chats unavailable: " + err.Error())
		return ""
	}
	return thread
}
