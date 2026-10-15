package chatcmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/agent-runner/agent-runner/internal/clisetup"
)

// handlePrompt shows, checks and previews the session prompt
// (MEMORY_DESIGN.md §6). Editing stays with /set-agent and /set-prompt.
func (c *Commander) handlePrompt(arg string) string {
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	systemPath, promptPath := c.rt.BootstrapPaths()
	files := []struct{ name, path string }{{"agent", systemPath}, {"workflow", promptPath}}

	switch strings.ToLower(sub) {
	case "", "show":
		var b strings.Builder
		for _, f := range files {
			if rest != "" && !strings.EqualFold(rest, f.name) {
				continue
			}
			data, err := os.ReadFile(f.path)
			switch {
			case os.IsNotExist(err):
				fmt.Fprintf(&b, "**%s** (%s): missing — /bootstrap creates it\n\n", f.name, f.path)
			case err != nil:
				fmt.Fprintf(&b, "**%s** (%s): %v\n\n", f.name, f.path, err)
			default:
				fmt.Fprintf(&b, "**%s** (%s, %d bytes)\n\n%s\n\n", f.name, f.path, len(data), strings.TrimSpace(string(data)))
			}
		}
		if b.Len() == 0 {
			return "error: usage: /prompt show [agent|workflow]"
		}
		return strings.TrimSpace(b.String())

	case "check":
		var b strings.Builder
		for _, f := range files {
			data, err := os.ReadFile(f.path)
			if err != nil {
				continue
			}
			for _, p := range clisetup.PromptCheck(string(data)) {
				fmt.Fprintf(&b, "- %s: %s\n", f.name, p)
			}
		}
		if b.Len() == 0 {
			return "ok: nothing in agent.md or prompt.md contradicts how the runner works now"
		}
		return "Out of date (the runner's own section of every prompt now covers these; edit with /set-agent or /set-prompt):\n" + b.String()

	case "preview":
		full := false
		if w, more, _ := strings.Cut(rest, " "); strings.EqualFold(w, "full") {
			full, rest = true, strings.TrimSpace(more)
		}
		if rest == "" {
			rest = "(your message)"
		}
		p, err := c.rt.PromptPreview(rest)
		if err != nil {
			return "error: " + err.Error()
		}
		if full {
			return p
		}
		return promptOutline(p)
	}
	return "error: usage: /prompt [show [agent|workflow]|check|preview [full] [message]]"
}

// promptOutline lists a composed prompt's sections with their sizes.
func promptOutline(p string) string {
	type sec struct {
		title string
		n     int
	}
	secs := []sec{{"(instructions before the first heading)", 0}}
	for _, line := range strings.SplitAfter(p, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "### ") {
			secs = append(secs, sec{t, 0})
		}
		secs[len(secs)-1].n += len(line)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A session would get %d characters:\n", len(p))
	for _, s := range secs {
		if s.n == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s — %d\n", s.title, s.n)
	}
	b.WriteString("(/prompt preview full shows all of it)")
	return b.String()
}
