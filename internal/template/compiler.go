package template

import (
	"fmt"
	"strings"
)

// Compile assembles the final prompt from input in order:
// system instructions → memory sections → recent conversation → current request.
func Compile(input PromptInput) string {
	var parts []string

	if s := substituteVars(input.SystemInstructions, input.Vars); s != "" {
		parts = append(parts, s)
	}

	var index []MemoryFile
	for _, f := range input.Retrieval.Files {
		if f.Indexed {
			index = append(index, f)
			continue
		}
		if f.Content == "" {
			continue
		}
		parts = append(parts, "## "+f.Name+"\n\n"+substituteVars(f.Content, input.Vars))
	}
	if s := memoryIndex(index, input.Vars[VarMemoryDir]); s != "" {
		parts = append(parts, s)
	}

	// Recent Sessions carries raw daily-log text (may contain literal {{...}}
	// from task messages) — no var substitution.
	if s := strings.TrimSpace(input.RecentSessions); s != "" {
		parts = append(parts, "## Recent Sessions\n\n"+s)
	}

	if req := substituteVars(input.CurrentRequest, input.Vars); req != "" {
		parts = append(parts, "## Current Request\n\n"+req)
	}

	return strings.Join(parts, "\n\n")
}

// maxIndexed bounds the memory-file index in a prompt.
const maxIndexed = 50

// memoryIndex lists the memory files not loaded into the prompt, with their
// size and first line, for the agent to read when a task touches them.
// The first lines are agent-written text: no var substitution.
func memoryIndex(files []MemoryFile, dir string) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Memory Files\n\nMore memory, not loaded here: read a file when the task touches its topic.\n")
	for i, f := range files {
		if i == maxIndexed {
			fmt.Fprintf(&b, "- ... and %d more (list the directory)\n", len(files)-maxIndexed)
			break
		}
		path := f.Filename
		if dir != "" {
			path = dir + "/" + f.Filename
		}
		line := "- " + path + " (" + humanSize(f.Size) + ")"
		if f.Content != "" {
			line += ": " + f.Content
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
