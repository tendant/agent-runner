package template

import "strings"

// Compile assembles the final prompt from input in order:
// system instructions → memory sections → recent conversation → current request.
func Compile(input PromptInput) string {
	var parts []string

	if s := substituteVars(input.SystemInstructions, input.Vars); s != "" {
		parts = append(parts, s)
	}

	for _, f := range input.Retrieval.Files {
		if f.Content == "" {
			continue
		}
		parts = append(parts, "## "+f.Name+"\n\n"+substituteVars(f.Content, input.Vars))
	}

	if len(input.RecentMessages) > 0 {
		parts = append(parts, "## Recent Conversation\n\n"+formatMessages(input.RecentMessages))
	}

	if req := substituteVars(input.CurrentRequest, input.Vars); req != "" {
		parts = append(parts, "## Current Request\n\n"+req)
	}

	return strings.Join(parts, "\n\n")
}

func formatMessages(msgs []Message) string {
	var lines []string
	for _, m := range msgs {
		lines = append(lines, strings.ToUpper(m.Role)+": "+m.Content)
	}
	return strings.Join(lines, "\n")
}
