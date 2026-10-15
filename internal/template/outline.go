package template

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Section is one part of a composed prompt: its heading, size and a short
// hash, so two sessions' prompts can be compared without storing them.
type Section struct {
	Title string
	Chars int
	Hash  string
}

// Outline splits a composed prompt at its "## " and "### " headings. Text
// before the first heading is "(instructions)".
func Outline(prompt string) []Section {
	type part struct {
		title string
		b     strings.Builder
	}
	parts := []*part{{title: "(instructions)"}}
	for _, line := range strings.SplitAfter(prompt, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "### ") {
			parts = append(parts, &part{title: t})
		}
		parts[len(parts)-1].b.WriteString(line)
	}
	var out []Section
	for _, p := range parts {
		s := p.b.String()
		if strings.TrimSpace(s) == "" {
			continue
		}
		h := sha256.Sum256([]byte(s))
		out = append(out, Section{Title: p.title, Chars: len(s), Hash: hex.EncodeToString(h[:4])})
	}
	return out
}
