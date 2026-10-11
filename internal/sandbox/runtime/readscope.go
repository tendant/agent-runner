package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// agentTools are the programs whose installs scoped reads must cover: the
// agent CLIs and node (claude, codex and pi are node programs when
// installed with npm).
var agentTools = []string{"claude", "codex", "pi", "opencode", "node"}

// toolReadable returns the directories the agent tools found on PATH run
// from: each executable's directory, the install prefix of a bin/ directory
// (nvm, ~/.local), and a node_modules tree (npm installs; codex's native
// binary is a sibling package there). System prefixes like /usr are readable
// anyway; these matter for tools installed under a home directory.
func toolReadable() []string {
	var homes []string // as set and with symlinks resolved (macOS /var -> /private/var)
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		homes = append(homes, filepath.Clean(h))
		if r, err := filepath.EvalSymlinks(h); err == nil {
			homes = append(homes, r)
		}
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		// Never the home directory or above: that would undo the scoping.
		for _, home := range homes {
			if p == home || strings.HasPrefix(home, p+string(filepath.Separator)) {
				return
			}
		}
		if p != "" && p != "/" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, name := range agentTools {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		p, _ = filepath.Abs(p)
		add(filepath.Dir(p))
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		dir := filepath.Dir(r)
		add(dir)
		if filepath.Base(dir) == "bin" {
			add(filepath.Dir(dir))
		}
		if i := strings.LastIndex(r, string(filepath.Separator)+"node_modules"+string(filepath.Separator)); i >= 0 {
			add(r[:i+len("/node_modules")])
		}
	}
	return out
}
