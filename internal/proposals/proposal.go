// Package proposals is the agent's way to change its own instructions
// (PROACTIVE_DESIGN.md §8–§9): a session writes a proposal to _proposals/,
// the runner validates and stores it and posts it for review, and only a
// person's approval writes it, as an operator edit committed on its own.
//
// What may be proposed is layer 1 and 2 only: agent.md, prompt.md, GOALS.md
// and skills under the memory dir. Budgets, caps, policy files, trust and the
// sandbox are not targets, so no proposal can widen what the agent may do.
package proposals

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Status of a proposal.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
	// StatusStale: the target changed after the proposal was written, so
	// it was not applied.
	StatusStale = "stale"
	// StatusUndone: applied, then taken back (/proposal undo).
	StatusUndone = "undone"
)

// MaxContent is the largest proposed file.
const MaxContent = 64 << 10

// Proposal is one suggested change to an instruction, goal or skill file.
type Proposal struct {
	ID       string   `json:"id"`
	Target   string   `json:"target"`         // agent.md | prompt.md | GOALS.md | skills/<name>/SKILL.md
	Base     string   `json:"base,omitempty"` // git blob hash of the target it was written against; "" for a new file
	Title    string   `json:"title"`
	Why      string   `json:"why"`
	Content  string   `json:"content"` // the full new content of the target
	Evidence []string `json:"evidence,omitempty"`

	SessionID string `json:"session_id"`
	Source    string `json:"source,omitempty"` // the session's source

	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	Decided   time.Time `json:"decided,omitzero"`
	DecidedBy string    `json:"decided_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Reminded  bool      `json:"reminded,omitempty"`
	// Previous is the target's content before it was applied, for undo.
	Previous    string    `json:"previous,omitempty"`
	PrevExisted bool      `json:"prev_existed,omitempty"`
	Eval        string    `json:"eval,omitempty"` // the latest eval comparison (/proposal eval)
	EvalAt      time.Time `json:"eval_at,omitzero"`
	Chat        string    `json:"chat,omitempty"`    // where it was posted
	Threads     []string  `json:"threads,omitempty"` // its thread, and its reminder's
}

// Targets says where each proposable file lives. agent.md and prompt.md are
// AGENT_SYSTEM_PROMPT and AGENT_PROMPT_FILE, which may be outside the memory
// dir; GOALS.md and skills are in it.
type Targets struct {
	SystemPrompt string // agent.md
	PromptFile   string // prompt.md
	MemoryDir    string
}

var skillTarget = regexp.MustCompile(`^skills/([a-z0-9][a-z0-9-]{0,63})/SKILL\.md$`)

// Path is the file target names, or an error when target may not be
// proposed.
func (t Targets) Path(target string) (string, error) {
	switch target {
	case "agent.md":
		if t.SystemPrompt == "" {
			return "", errors.New("agent.md has no path here (AGENT_SYSTEM_PROMPT)")
		}
		return t.SystemPrompt, nil
	case "prompt.md":
		if t.PromptFile == "" {
			return "", errors.New("prompt.md has no path here (AGENT_PROMPT_FILE)")
		}
		return t.PromptFile, nil
	case "GOALS.md":
		if t.MemoryDir == "" {
			return "", errors.New("no memory dir")
		}
		return filepath.Join(t.MemoryDir, "GOALS.md"), nil
	}
	if skillTarget.MatchString(target) {
		if t.MemoryDir == "" {
			return "", errors.New("no memory dir")
		}
		return filepath.Join(t.MemoryDir, filepath.FromSlash(target)), nil
	}
	return "", fmt.Errorf("%q cannot be proposed: only agent.md, prompt.md, GOALS.md and skills/<name>/SKILL.md can", target)
}

// SkillName is the skill a target names, or "".
func SkillName(target string) string {
	if m := skillTarget.FindStringSubmatch(target); m != nil {
		return m[1]
	}
	return ""
}

// BlobHash is content's git blob hash (what `git hash-object` prints), so
// an agent with git can compute a base itself.
func BlobHash(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// sameBase reports whether a base an agent wrote (any prefix of 7 or more)
// names the hash full.
func sameBase(base, full string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return len(base) >= 7 && strings.HasPrefix(full, base)
}

// Parse reads a proposal file:
//
//	---
//	target: prompt.md
//	base: 3f9c2a1
//	title: Run the linter before committing Go changes
//	evidence: [sess-81c2, sess-90af]
//	---
//
//	## Why
//	...
//
//	## Change
//	<the full new content of the target>
func Parse(data string) (*Proposal, error) {
	data = strings.TrimPrefix(strings.ReplaceAll(data, "\r\n", "\n"), "\ufeff")
	if !strings.HasPrefix(data, "---\n") {
		return nil, errors.New("no front matter (it starts with a --- line, then target:, base:, title:)")
	}
	head, body, ok := strings.Cut(data[4:], "\n---")
	if !ok {
		return nil, errors.New("front matter is not closed with a --- line")
	}
	body = strings.TrimPrefix(body, "-") // a ---- line
	p := &Proposal{}
	for _, line := range strings.Split(head, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = unquote(strings.TrimSpace(stripComment(val)))
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "target":
			p.Target = strings.TrimPrefix(val, "./")
		case "base":
			p.Base = strings.ToLower(val)
		case "title":
			p.Title = val
		case "evidence":
			for _, e := range strings.Split(strings.Trim(val, "[]"), ",") {
				if e = unquote(strings.TrimSpace(e)); e != "" {
					p.Evidence = append(p.Evidence, e)
				}
			}
		}
	}
	why, change, err := sections(body)
	if err != nil {
		return nil, err
	}
	p.Why, p.Content = why, change
	switch {
	case p.Target == "":
		return nil, errors.New("no target:")
	case p.Title == "":
		return nil, errors.New("no title:")
	case strings.TrimSpace(p.Content) == "":
		return nil, errors.New("## Change is empty")
	}
	if looksLikeDiff(p.Content) {
		return nil, errors.New("## Change is a diff: give the full new content of the file instead")
	}
	return p, nil
}

// sections splits a proposal body into its ## Why and ## Change parts. The
// change is everything after the ## Change line, so the new content may
// have headings of its own; one fenced block around all of it is unwrapped.
func sections(body string) (why, change string, err error) {
	lines := strings.Split(body, "\n")
	whyAt, changeAt := -1, -1
	for i, l := range lines {
		h := strings.ToLower(strings.TrimSpace(l))
		if whyAt < 0 && (h == "## why" || h == "# why") {
			whyAt = i
		}
		if h == "## change" || h == "# change" {
			changeAt = i
			break
		}
	}
	if changeAt < 0 {
		return "", "", errors.New("no ## Change section")
	}
	if whyAt >= 0 {
		why = strings.TrimSpace(strings.Join(lines[whyAt+1:changeAt], "\n"))
	}
	rest := lines[changeAt+1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	for len(rest) > 0 && strings.TrimSpace(rest[len(rest)-1]) == "" {
		rest = rest[:len(rest)-1]
	}
	if len(rest) >= 2 && strings.HasPrefix(rest[0], "```") {
		fence := rest[0][:len(rest[0])-len(strings.TrimLeft(rest[0], "`"))]
		if strings.TrimSpace(rest[len(rest)-1]) == fence {
			rest = rest[1 : len(rest)-1]
		}
	}
	if len(rest) == 0 {
		return why, "", nil
	}
	return why, strings.Join(rest, "\n") + "\n", nil
}

func looksLikeDiff(s string) bool {
	first := strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	return strings.HasPrefix(first, "diff --git ") || strings.HasPrefix(first, "@@ ") ||
		(strings.HasPrefix(first, "--- ") && strings.Contains(s, "\n+++ "))
}

func stripComment(v string) string {
	if i := strings.Index(v, " #"); i >= 0 {
		return v[:i]
	}
	return v
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}
