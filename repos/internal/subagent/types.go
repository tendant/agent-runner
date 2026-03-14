package subagent

// PlanStep represents one step in a structured plan.
type PlanStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Files       []string `json:"files,omitempty"`
	Done        bool     `json:"done"`
}

// PlanResult is the structured output from the planner sub-agent.
type PlanResult struct {
	Summary   string     `json:"summary"`
	Steps     []PlanStep `json:"steps"`
	Approach  string     `json:"approach"`
	RawOutput string     `json:"-"`
}

// ReviewResult is the structured output from the reviewer sub-agent (phase 2).
type ReviewResult struct {
	Complete    bool     `json:"complete"`
	Score       int      `json:"score"`
	Issues      []string `json:"issues,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
	RawOutput   string   `json:"-"`
}

// WorkspaceState captures a snapshot of filesystem and git state in the workspace.
type WorkspaceState struct {
	TodoContent   string   `json:"todo_content,omitempty"`
	RecentCommits []string `json:"recent_commits,omitempty"`
	RepoNames     []string `json:"repo_names,omitempty"`
	GitDiffStat   string   `json:"git_diff_stat,omitempty"`
}
