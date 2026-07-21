package template

// MemoryFile is a named memory section loaded during retrieval.
type MemoryFile struct {
	Name      string
	Filename  string // source filename in the memory dir, e.g. "decisions.md"
	Content   string
	WellKnown bool // curated files (user_preferences, decisions, ...) — trimmed last by ApplyBudget
}

// Retrieval holds the memory sections selected for a prompt.
type Retrieval struct {
	Files []MemoryFile
}

// PromptInput is everything the compiler needs to build a prompt.
type PromptInput struct {
	SystemInstructions string
	Retrieval          Retrieval
	RecentSessions     string // pre-formatted daily-log digest (see RecentSessions); no var substitution
	CurrentRequest     string
	Vars               map[string]string // {{KEY}} substituted in all text fields except RecentSessions
}
