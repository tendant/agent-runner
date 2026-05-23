package template

import "time"

// Message is a single conversation turn.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
	Time    time.Time
}

// MemoryFile is a named memory section loaded during retrieval.
type MemoryFile struct {
	Name    string
	Content string
}

// Retrieval holds the memory sections selected for a prompt.
type Retrieval struct {
	Files []MemoryFile
}

// PromptInput is everything the compiler needs to build a prompt.
type PromptInput struct {
	SystemInstructions string
	Retrieval          Retrieval
	RecentMessages     []Message
	CurrentRequest     string
	Vars               map[string]string // {{KEY}} substituted in all text fields
}
