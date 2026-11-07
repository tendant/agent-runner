package execution

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/proposals"
	"github.com/agent-runner/agent-runner/internal/safefs"
)

// proposalsDir is where a session leaves proposals to change its own
// instructions (PROACTIVE_DESIGN.md §9), collected at the end like _send/.
const proposalsDir = "_proposals"

// maxProposalFiles bounds what is read from _proposals/; the service keeps
// at most proposals.MaxPerSession of them.
const maxProposalFiles = 10

// ProposalSink takes a session's proposals for review (internal/proposals).
type ProposalSink interface {
	Enabled() bool
	Submit(ctx context.Context, sub proposals.Submission) []proposals.Note
}

// proposalsOn reports whether sessions are told about _proposals/.
func (h *Engine) proposalsOn() bool { return h.proposals != nil && h.proposals.Enabled() }

// collectProposals hands the .md files in _proposals/ to review. What was
// dropped is a session warning; what was stored, a note on its result.
func (h *Engine) collectProposals(ctx context.Context, sessionID string, live *agent.Session, checkoutPath string) {
	dir := filepath.Join(checkoutPath, proposalsDir)
	if _, err := os.Lstat(dir); err != nil {
		return
	}
	if h.proposals == nil {
		return
	}
	root, err := safefs.Open(dir)
	if err != nil {
		live.AddWarning("_proposals/: " + err.Error())
		return
	}
	defer root.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		live.AddWarning("_proposals/: " + err.Error())
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var files []proposals.File
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if len(files) == maxProposalFiles {
			live.AddWarning("_proposals/: only the first 10 files were read")
			break
		}
		data, err := safefs.ReadIn(root, e.Name(), proposals.MaxContent+8<<10)
		if err != nil {
			live.AddWarning("_proposals/" + e.Name() + ": " + err.Error())
			continue
		}
		files = append(files, proposals.File{Name: e.Name(), Data: data})
	}
	snap := live.Snapshot()
	notes := h.proposals.Submit(ctx, proposals.Submission{
		SessionID: sessionID,
		Source:    snap.Source,
		Files:     files,
		Secret:    func(text string) bool { return live.Redact(text) != text },
	})
	for _, n := range notes {
		if n.Dropped {
			live.AddWarning(n.Text)
		} else {
			live.AddNote(n.Text)
		}
	}
	if len(files) > 0 {
		slog.Info("proposals collected", "session_id", sessionID, "files", len(files), "notes", len(notes))
	}
}

// proposalTarget is a file a session may propose a change to, with the
// base it would propose against ("" when it does not exist yet).
type proposalTarget struct {
	Target string
	Base   string
}

// proposalTargets is what the runtime layer lists: the instructions, the
// goals and the agent's own skills, with their current bases.
func (h *Engine) proposalTargets() []proposalTarget {
	if !h.proposalsOn() {
		return nil
	}
	systemPath, promptPath := h.deps.BootstrapPaths()
	tg := proposals.Targets{SystemPrompt: systemPath, PromptFile: promptPath, MemoryDir: h.config.MemoryDir}
	names := []string{"agent.md", "prompt.md", "GOALS.md"}
	if h.config.MemoryDir != "" {
		entries, _ := os.ReadDir(filepath.Join(h.config.MemoryDir, "skills"))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, "skills/"+e.Name()+"/SKILL.md")
			}
		}
	}
	var out []proposalTarget
	for _, n := range names {
		path, err := tg.Path(n)
		if err != nil {
			continue
		}
		t := proposalTarget{Target: n}
		if data, err := os.ReadFile(path); err == nil {
			t.Base = proposals.BlobHash(data)[:12]
		}
		out = append(out, t)
	}
	return out
}
