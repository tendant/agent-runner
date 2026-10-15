package agentsecrets

import (
	"os"
	"sort"
	"strings"
)

// minRedactLen keeps short values (a "1", a "true") from being redacted
// everywhere they appear.
const minRedactLen = 8

// Redactor replaces known secret values with [redacted:NAME] in text the
// runner records or forwards: what an agent said (chat, API, audit log),
// the daily log and memory notes (pushed to the memory remote). An agent
// holds the users' secrets by design; this keeps them from being copied
// further than the agent itself.
type Redactor struct{ r *strings.Replacer }

// NewRedactor redacts the given agent secrets and agent-runner's own
// credentials in its environment (names ending in _TOKEN, _KEY, _SECRET,
// _PASSWORD, and API_KEY).
func NewRedactor(secrets []Secret) *Redactor {
	type pair struct{ value, name string }
	var pairs []pair
	seen := map[string]bool{}
	add := func(name, value string) {
		if len(value) >= minRedactLen && !seen[value] {
			seen[value] = true
			pairs = append(pairs, pair{value, name})
		}
	}
	for _, s := range secrets {
		add(s.Name, s.Value)
	}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if runnerCredential(name) {
			add(name, value)
		}
	}
	if len(pairs) == 0 {
		return &Redactor{}
	}
	// Longest first: a value containing another is replaced whole.
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	args := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		args = append(args, p.value, "[redacted:"+p.name+"]")
	}
	return &Redactor{r: strings.NewReplacer(args...)}
}

// Redact returns s with every known secret value replaced.
func (r *Redactor) Redact(s string) string {
	if r == nil || r.r == nil || s == "" {
		return s
	}
	return r.r.Replace(s)
}

func runnerCredential(name string) bool {
	if name == "API_KEY" {
		return true
	}
	for _, suf := range []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}
