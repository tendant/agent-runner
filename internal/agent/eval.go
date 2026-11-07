package agent

// EvalSpec makes a session an eval replay (PROACTIVE_DESIGN.md §11): it
// runs a past task as an ordinary session would, with Overrides in place of
// the instruction files they name, and leaves nothing behind. Its memory
// changes are not merged; no daily log, curation or push; nothing is
// pushed, sent or scheduled; no chat is told. When it ends, Checks are run
// against what it did, and their results are recorded on the session.
type EvalSpec struct {
	// Overrides maps a target (agent.md, prompt.md, GOALS.md,
	// skills/<name>/SKILL.md) to the content the replay uses instead.
	Overrides map[string]string
	Checks    []EvalCheck
}

// EvalCheck is one thing an eval replay must have done.
type EvalCheck struct {
	Kind string `json:"kind"` // file (exists in the workspace), command (exits 0 in the workspace, sandboxed), reply (the agent's reply contains it)
	Arg  string `json:"arg"`
}

// EvalResult is how one check went.
type EvalResult struct {
	Check  EvalCheck `json:"check"`
	Pass   bool      `json:"pass"`
	Detail string    `json:"detail,omitempty"`
}

// SetEvalResults records an eval replay's check results.
func (s *Session) SetEvalResults(r []EvalResult) {
	s.mu.Lock()
	s.EvalResults = append([]EvalResult(nil), r...)
	s.mu.Unlock()
}
