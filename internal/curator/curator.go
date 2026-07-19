// Package curator runs a post-session memory curation pass: a cheap LLM call
// that distills each session's outcome into durable lessons and compacts
// memory files that have outgrown their budget. It writes only to an
// allowlisted set of files inside the memory dir and is always non-fatal to
// the calling session — the git-backed memory history is the undo mechanism.
package curator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/llm"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// LessonsFile is the curator's own output file. The agent may append to it
// during sessions but the curator owns compaction.
const LessonsFile = "lessons.md"

// compactable lists the only files the curator may rewrite, and only when
// they exceed their per-file budget. agent.md, prompt.md, and daily logs are
// deliberately absent — the curator never touches them.
var compactable = []string{
	"user_preferences.md",
	"project_summary.md",
	"decisions.md",
	"workflows.md",
	LessonsFile,
}

// maxPromptInput caps the total characters of memory-file content included in
// the curation prompt.
const maxPromptInput = 16000

// maxLessonAppend caps a single lessons_append payload.
const maxLessonAppend = 1000

// Input describes the finished session the curator distills.
type Input struct {
	TaskPreview       string // first ~80 chars of the task message
	Status            string
	Error             string
	ChangedFiles      []string
	ReviewScore       int
	ReviewIssues      []string
	ReviewSuggestions []string
	HasReview         bool
}

// Config configures a Curator.
type Config struct {
	MemoryDir string
	CharCap   int // AGENT_MEMORY_CHAR_CAP; per-file budget derived as CharCap/3, 0 disables compaction
	Timeout   time.Duration
}

// Summary reports what a Curate call changed.
type Summary struct {
	LessonAppended bool
	FilesCompacted []string
}

// Curator distills session outcomes into memory via an LLM client.
type Curator struct {
	client llm.Client
	cfg    Config
}

// New creates a Curator. client must be non-nil.
func New(client llm.Client, cfg Config) *Curator {
	return &Curator{client: client, cfg: cfg}
}

const curationPromptHeader = `You maintain an AI agent's long-term memory files. Given a session outcome and the current memory file contents below:

1. Extract at most 2 durable lessons (mistakes to avoid, preferences learned, project facts confirmed). Skip session-specific noise. If nothing durable was learned, use an empty string.
2. For each file marked OVER BUDGET, produce a compacted rewrite: merge duplicates, drop stale or superseded entries, keep every distinct fact. The rewrite MUST be shorter than the original.

Respond with ONLY JSON, no other text:
{"lessons_append": "- <lesson>", "compact": [{"file": "<name.md>", "content": "<full rewritten content>"}]}

Rules: "compact" may only name files listed as OVER BUDGET below. Use {"lessons_append": "", "compact": []} when there is nothing to do.`

// Curate runs one distill-and-compact pass. All failures are returned for
// logging; the caller treats them as non-fatal. Files are written directly to
// the memory dir — the caller's subsequent git push persists them.
func (c *Curator) Curate(ctx context.Context, in Input) (Summary, error) {
	var summary Summary
	if c.client == nil {
		return summary, fmt.Errorf("no LLM client configured")
	}

	overBudget := c.overBudgetFiles()
	prompt := c.buildPrompt(in, overBudget)

	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	output, err := c.client.Complete(ctx, prompt)
	if err != nil {
		return summary, fmt.Errorf("curation LLM call: %w", err)
	}

	resp, err := parseCuration(output)
	if err != nil {
		return summary, err
	}

	// Apply the lesson append.
	if lesson := strings.TrimSpace(resp.LessonsAppend); lesson != "" {
		if len(lesson) > maxLessonAppend {
			lesson = lesson[:maxLessonAppend]
		}
		if err := c.appendLesson(lesson); err != nil {
			slog.Warn("curator: lesson append failed", "error", err)
		} else {
			summary.LessonAppended = true
		}
	}

	// Apply compactions, each individually validated.
	for _, item := range resp.Compact {
		orig, ok := overBudget[item.File]
		if !ok {
			slog.Warn("curator: rejected compaction of file not over budget", "file", item.File)
			continue
		}
		content := strings.TrimSpace(item.Content)
		if content == "" || len(content) >= len(orig) {
			slog.Warn("curator: rejected compaction that does not shrink the file", "file", item.File)
			continue
		}
		if err := tmpl.WriteMemoryFile(c.cfg.MemoryDir, item.File, content+"\n"); err != nil {
			slog.Warn("curator: compaction write failed", "file", item.File, "error", err)
			continue
		}
		summary.FilesCompacted = append(summary.FilesCompacted, item.File)
	}

	return summary, nil
}

// overBudgetFiles returns allowlisted files currently exceeding the per-file
// budget, mapped to their current content. Empty when CharCap is 0.
func (c *Curator) overBudgetFiles() map[string]string {
	over := make(map[string]string)
	if c.cfg.CharCap <= 0 {
		return over
	}
	perFileBudget := c.cfg.CharCap / 3
	for _, name := range compactable {
		content, err := tmpl.ReadMemoryFile(c.cfg.MemoryDir, name)
		if err != nil {
			continue
		}
		if len(content) > perFileBudget {
			over[name] = content
		}
	}
	return over
}

// buildPrompt assembles the curation prompt: header, session outcome, then
// each allowlisted file's content with a budget annotation, capped in total.
func (c *Curator) buildPrompt(in Input, overBudget map[string]string) string {
	var b strings.Builder
	b.WriteString(curationPromptHeader)
	b.WriteString("\n\n## Session Outcome\n\n")
	fmt.Fprintf(&b, "Task: %s\nStatus: %s\n", in.TaskPreview, in.Status)
	if in.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", in.Error)
	}
	if len(in.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "Files changed: %s\n", strings.Join(in.ChangedFiles, ", "))
	}
	if in.HasReview {
		fmt.Fprintf(&b, "Review score: %d/10\n", in.ReviewScore)
		if len(in.ReviewIssues) > 0 {
			fmt.Fprintf(&b, "Review issues: %s\n", strings.Join(in.ReviewIssues, "; "))
		}
		if len(in.ReviewSuggestions) > 0 {
			fmt.Fprintf(&b, "Review suggestions: %s\n", strings.Join(in.ReviewSuggestions, "; "))
		}
	}

	b.WriteString("\n## Memory Files\n")
	perFileBudget := 0
	if c.cfg.CharCap > 0 {
		perFileBudget = c.cfg.CharCap / 3
	}
	remaining := maxPromptInput
	for _, name := range compactable {
		content, err := tmpl.ReadMemoryFile(c.cfg.MemoryDir, name)
		if err != nil {
			continue
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if len(content) > remaining {
			content = content[:max(remaining, 0)]
		}
		remaining -= len(content)
		if _, isOver := overBudget[name]; isOver {
			fmt.Fprintf(&b, "\n### %s [OVER BUDGET: %d/%d chars — compact this]\n\n%s\n", name, len(content), perFileBudget, content)
		} else {
			fmt.Fprintf(&b, "\n### %s [within budget — do not compact]\n\n%s\n", name, content)
		}
		if remaining <= 0 {
			break
		}
	}
	return b.String()
}

// appendLesson appends a dated lesson entry to lessons.md.
func (c *Curator) appendLesson(lesson string) error {
	existing, _ := tmpl.ReadMemoryFile(c.cfg.MemoryDir, LessonsFile)
	entry := fmt.Sprintf("### %s\n%s\n", time.Now().Format("2006-01-02"), lesson)
	var content string
	if strings.TrimSpace(existing) == "" {
		content = "# Lessons\n\n" + entry
	} else {
		content = strings.TrimRight(existing, "\n") + "\n\n" + entry
	}
	return tmpl.WriteMemoryFile(c.cfg.MemoryDir, LessonsFile, content)
}
