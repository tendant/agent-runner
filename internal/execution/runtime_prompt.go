package execution

import (
	"path/filepath"
	"strings"
)

// The runtime layer of a session's prompt (MEMORY_DESIGN.md §3): how this
// runner works, generated per session from the code that does the work, so
// it never goes stale the way a seeded agent.md does. agent.md keeps only
// what the operator knows (persona, house rules).

// runtimeFacts is what the runtime layer depends on.
type runtimeFacts struct {
	Sandboxed  bool
	MemoryDir  string // as the agent sees it
	MemoryCopy bool   // MemoryDir is a staged copy, merged back at the end
}

func runtimeInstructions(f runtimeFacts) string {
	var b strings.Builder
	b.WriteString("\n\n## How this runner works\n\n")
	b.WriteString("Written by agent-runner for this session. Where anything above disagrees, this wins.\n")

	b.WriteString("\n### Git\n\n")
	if f.Sandboxed {
		b.WriteString("You have no git credentials here, by design: commit your work, but do not `git push`,\n" +
			"`git pull` or `git fetch` against the remote, and do not set up credentials or new remotes.\n" +
			"When you finish, the runner pushes your commits to the branch you are on. If another session\n" +
			"pushed meanwhile, it rebases them for you, and asks you to resolve the conflicts only if\n" +
			"that rebase conflicts. Repos you clone or create yourself are not pushed.\n")
	} else {
		b.WriteString("Commit your work with descriptive messages and push it to origin. Other sessions may push to\n" +
			"the same branch: if your push is rejected, run `git pull --rebase`, resolve conflicts keeping\n" +
			"both sides' intent, and push again. Never force-push or reset a shared branch.\n")
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
			"- `lessons.md`: append only; curated after each session\n" +
			"- any other lowercase `.md` file, for a topic of its own\n\n" +
			"Append; delete only to correct an error. `agent.md` and `prompt.md` are your instructions, set by\n" +
			"the operator: do not edit them; propose changes in your reply instead.\n")
		if f.MemoryCopy {
			b.WriteString("\nThis directory is your own copy. Changes to its lowercase `.md` files (new ones too) are\n" +
				"saved when the session ends; `agent.md`, `prompt.md`, upper-case files such as `HEARTBEAT.md`,\n" +
				"daily logs, deletions, subdirectories and links are not.\n")
		}
	}

	b.WriteString("\n### Credentials\n\n")
	b.WriteString("Credentials the operator gives you are environment variables: use them from there, never search\n" +
		"files for keys, and never print or write them out.")
	if f.Sandboxed {
		b.WriteString(" Only the variables the operator passed in are set here,\n" +
			"and model API keys never are: your CLI reaches the model through the runner.")
	}
	b.WriteString("\n")

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
	return p + runtimeInstructions(runtimeFacts{Sandboxed: h.sandboxed(), MemoryDir: h.config.MemoryDir, MemoryCopy: h.sandboxed()}), nil
}
