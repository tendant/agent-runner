package template

import "fmt"

// truncationNotice is appended to a file's content when it is cut to fit the
// budget, pointing the agent at the full file on disk.
const truncationNotice = "\n\n[... truncated to fit the memory budget — full content in %s]"

// ApplyBudget trims a Retrieval so the total content size is at most budget
// characters (budget <= 0 means unlimited). Files on disk are never modified —
// this shapes only what goes into the prompt. Policy, in order:
//
//  1. Per-file cap: any single file larger than budget/3 is truncated to
//     budget/3 (head kept, truncation notice appended).
//  2. If the total still exceeds budget: drop non-well-known files,
//     last-loaded first.
//  3. If still over: truncate well-known files in reverse load order
//     (workflows before decisions before project_summary before
//     user_preferences), since earlier files are the most curated.
//
// Returns the trimmed retrieval and a human-readable warning per action taken.
func ApplyBudget(r Retrieval, budget int) (Retrieval, []string) {
	if budget <= 0 || totalSize(r) <= budget {
		return r, nil
	}

	var warnings []string
	files := make([]MemoryFile, len(r.Files))
	copy(files, r.Files)

	// 1. Per-file cap — misc files only, so one bloated scratch file can't
	// starve the rest. Well-known curated files are only ever trimmed by
	// step 3, in reverse priority order.
	perFileCap := budget / 3
	if perFileCap > 0 {
		for i := range files {
			if !files[i].WellKnown && len(files[i].Content) > perFileCap {
				files[i].Content = truncate(files[i].Content, perFileCap) +
					fmt.Sprintf(truncationNotice, files[i].Filename)
				warnings = append(warnings, fmt.Sprintf("truncated %s to %d chars (per-file cap)", files[i].Filename, perFileCap))
			}
		}
	}

	// 2. Drop non-well-known files, last-loaded first.
	for i := len(files) - 1; i >= 0 && totalSize(Retrieval{Files: files}) > budget; i-- {
		if files[i].WellKnown {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("dropped %s (over memory budget)", files[i].Filename))
		files = append(files[:i], files[i+1:]...)
	}

	// 3. Truncate well-known files in reverse load order until we fit.
	for i := len(files) - 1; i >= 0; i-- {
		over := totalSize(Retrieval{Files: files}) - budget
		if over <= 0 {
			break
		}
		// The notice appended below adds length; trim enough to cover it too.
		notice := fmt.Sprintf(truncationNotice, files[i].Filename)
		keep := max(len(files[i].Content)-over-len(notice), 0)
		files[i].Content = truncate(files[i].Content, keep) + notice
		warnings = append(warnings, fmt.Sprintf("truncated %s to %d chars (over memory budget)", files[i].Filename, keep))
	}

	return Retrieval{Files: files}, warnings
}

// truncate cuts s to at most n characters without splitting a line mid-way
// when possible: it prefers the last newline within the limit.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := lastNewline(cut); i > n/2 {
		cut = cut[:i]
	}
	return cut
}

func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

func totalSize(r Retrieval) int {
	n := 0
	for _, f := range r.Files {
		n += len(f.Content)
	}
	return n
}
