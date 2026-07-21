// Package textutil holds small text helpers shared across packages.
package textutil

import "strings"

// Truncate returns s unchanged when it has at most n runes; otherwise the
// first n runes with "..." appended. Rune-safe: never splits a multi-byte
// character (unlike the bespoke s[:n] slicing it replaces).
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// ExtractJSON extracts a JSON object from LLM output: the whole (trimmed)
// string when it starts with "{", otherwise the span from the first "{" to
// the last "}". ok is false when no object-shaped span exists. Callers still
// json.Unmarshal the result — this only locates the candidate span.
func ExtractJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s, true
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return s[start : end+1], true
}
