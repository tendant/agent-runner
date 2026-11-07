package proposals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// Limits (PROACTIVE_DESIGN.md §9.2, §10).
const (
	MaxPendingPerTarget = 3
	MaxPending          = 10
	MaxPerSession       = 3
	RemindAfter         = 3 * 24 * time.Hour
	ExpireAfter         = 14 * 24 * time.Hour
	KeepDecided         = 90 * 24 * time.Hour // a decided proposal's record is then removed
)

// Chat posts a proposal (or its reminder) as a new thread in target and
// returns the thread's key, "" when the chat has no threads to reply in.
//
// PostInThread posts message in thread (one OpenProposalThread returned),
// or in target's chat when thread is "".
type Chat interface {
	OpenProposalThread(ctx context.Context, target, message string) (thread string, err error)
	PostInThread(ctx context.Context, target, thread, message string) error
}

// Evaluator replays the eval set with the current instructions and with a
// proposal's (internal/evals). Check says how many evals a comparison would
// replay, or why it cannot run now.
type Evaluator interface {
	Check() (int, error)
	Compare(ctx context.Context, overrides map[string]string) (report string, err error)
}

// Config wires a Service.
type Config struct {
	StateDir string           // STATE_ROOT/proposals
	Targets  func() Targets   // read per call: /config set may move them
	Chat     Chat             // nil: stored and listed, not posted
	Where    func() string    // AGENT_PROACTIVE_CHAT; "" = not posted
	Enabled  func() bool      // AGENT_PROPOSALS
	Eval     Evaluator        // nil: no /proposal eval
	Now      func() time.Time // tests; default time.Now
}

// Service validates, stores and decides proposals.
type Service struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Enabled == nil {
		cfg.Enabled = func() bool { return true }
	}
	if cfg.Where == nil {
		cfg.Where = func() string { return "" }
	}
	return &Service{cfg: cfg}
}

// Enabled reports whether sessions may propose (AGENT_PROPOSALS).
func (s *Service) Enabled() bool { return s.cfg.Enabled() }

// File is one file a session left in _proposals/.
type File struct {
	Name string
	Data []byte
}

// Submission is what one session proposed.
type Submission struct {
	SessionID string
	Source    string
	Files     []File
	// Secret reports whether text holds a secret value (the session's
	// redactor changed it); a proposal that does is dropped.
	Secret func(text string) bool
}

// Note is what became of one proposal file.
type Note struct {
	Text    string
	Dropped bool // invalid, or over a limit: a problem for the session to report
}

// Submit validates and stores what a session proposed and posts each one
// that is valid. It returns a note per file.
func (s *Service) Submit(ctx context.Context, sub Submission) []Note {
	if len(sub.Files) == 0 {
		return nil
	}
	if !s.Enabled() {
		slog.Info("proposals: dropped, AGENT_PROPOSALS is off", "session_id", sub.SessionID, "files", len(sub.Files))
		return []Note{{Text: fmt.Sprintf("_proposals/: %d dropped (AGENT_PROPOSALS is off)", len(sub.Files)), Dropped: true}}
	}
	var notes []Note
	var accepted []string
	var stored []*Proposal
	s.mu.Lock()
	for _, f := range sub.Files {
		if len(accepted) >= MaxPerSession {
			notes = append(notes, Note{"_proposals/" + f.Name + ": dropped: at most 3 proposals per session", true})
			continue
		}
		p, err := s.validate(f, sub)
		if err != nil {
			notes = append(notes, Note{"_proposals/" + f.Name + ": dropped: " + err.Error(), true})
			slog.Info("proposals: dropped", "session_id", sub.SessionID, "file", f.Name, "reason", err)
			continue
		}
		if err := s.save(p); err != nil {
			notes = append(notes, Note{"_proposals/" + f.Name + ": not stored: " + err.Error(), true})
			continue
		}
		accepted = append(accepted, p.ID)
		stored = append(stored, p)
	}
	s.mu.Unlock()
	for _, p := range stored {
		where := s.post(ctx, p, s.message(p))
		notes = append(notes, Note{Text: fmt.Sprintf("Proposal %s (`%s`): %s — %s.", p.ID, p.Target, p.Title, where)})
	}
	return notes
}

// validate parses one file and checks it against the target and the
// pending proposals (PROACTIVE_DESIGN.md §9.2). Called under s.mu.
func (s *Service) validate(f File, sub Submission) (*Proposal, error) {
	if len(f.Data) > MaxContent+8<<10 {
		return nil, fmt.Errorf("over %d KiB", MaxContent>>10)
	}
	p, err := Parse(string(f.Data))
	if err != nil {
		return nil, err
	}
	if len(p.Content) > MaxContent {
		return nil, fmt.Errorf("the new content is over %d KiB", MaxContent>>10)
	}
	path, err := s.cfg.Targets().Path(p.Target)
	if err != nil {
		return nil, err
	}
	current, exists, err := readTarget(path)
	if err != nil {
		return nil, err
	}
	hash := BlobHash([]byte(current))
	switch {
	case p.Base != "" && !exists:
		return nil, fmt.Errorf("base %s, but %s does not exist (omit base: for a new file)", p.Base, p.Target)
	case p.Base != "" && !sameBase(p.Base, hash):
		return nil, fmt.Errorf("base %s no longer matches %s (now %s): it changed while you worked", p.Base, p.Target, hash[:7])
	}
	if exists {
		p.Base = hash
	}
	if p.Content == current {
		return nil, errors.New("no change to " + p.Target)
	}
	if sub.Secret != nil && sub.Secret(p.Title+"\n"+p.Why+"\n"+p.Content) {
		return nil, errors.New("it contains a secret value")
	}
	all := s.list()
	var pending, forTarget int
	for _, q := range all {
		if q.Status != StatusPending {
			continue
		}
		pending++
		if q.Target == p.Target {
			forTarget++
		}
	}
	if forTarget >= MaxPendingPerTarget {
		return nil, fmt.Errorf("%d proposals for %s are already waiting for review", forTarget, p.Target)
	}
	if pending >= MaxPending {
		return nil, fmt.Errorf("%d proposals are already waiting for review", pending)
	}
	p.ID = newID()
	p.SessionID, p.Source = sub.SessionID, sub.Source
	p.Status = StatusPending
	p.Created = s.cfg.Now().UTC()
	return p, nil
}

// readTarget reads a target file; a missing one is "" and !exists.
func readTarget(path string) (content string, exists bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// post sends a proposal's message to AGENT_PROACTIVE_CHAT and records the
// thread; it says where it went.
func (s *Service) post(ctx context.Context, p *Proposal, msg string) string {
	where := s.cfg.Where()
	if where == "" || s.cfg.Chat == nil {
		return "waiting for review (/proposals; set AGENT_PROACTIVE_CHAT to have them posted)"
	}
	thread, err := s.cfg.Chat.OpenProposalThread(ctx, where, msg)
	if err != nil {
		slog.Warn("proposals: could not post", "id", p.ID, "chat", where, "error", err)
		return "waiting for review (/proposals; posting it failed: " + err.Error() + ")"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, err := s.load(p.ID); err == nil {
		p = cur
	}
	p.Chat = where
	if thread != "" {
		p.Threads = append(p.Threads, thread)
	}
	_ = s.save(p)
	return "posted for review in " + where
}

// maxChatDiff bounds the diff in a chat message; /proposal show <id> full
// gives all of it.
const maxChatDiff = 6000

// message is a proposal as posted for review.
func (s *Service) message(p *Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Proposal %s: %s**\n\n", p.ID, p.Title)
	fmt.Fprintf(&b, "Changes `%s`, from session %s", p.Target, p.SessionID)
	if len(p.Evidence) > 0 {
		fmt.Fprintf(&b, " · evidence: %s", strings.Join(p.Evidence, ", "))
	}
	b.WriteString("\n\n")
	if p.Why != "" {
		b.WriteString(p.Why + "\n\n")
	}
	b.WriteString("```diff\n" + clip(s.diff(p), maxChatDiff) + "```\n\n")
	if p.Eval != "" {
		b.WriteString("**Eval** (" + p.EvalAt.Local().Format("Jan 2 15:04") + "):\n\n" + p.Eval + "\n\n")
	}
	fmt.Fprintf(&b, "Reply **yes** to apply it, or **no** and why to reject it (`/proposal approve %s`, `/proposal reject %s <reason>`).", p.ID, p.ID)
	return b.String()
}

func (s *Service) diff(p *Proposal) string {
	var current string
	if path, err := s.cfg.Targets().Path(p.Target); err == nil {
		current, _, _ = readTarget(path)
	}
	return Diff(current, p.Content, p.Target)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], "\n")
	if cut < 0 {
		cut = n
	}
	return s[:cut+1] + "… (diff clipped; /proposal show <id> full)\n"
}

// Approve applies a pending proposal: the target is written as an operator
// edit, committed alone (memory: <target> from proposal <id> ...). by names
// who approved it.
func (s *Service) Approve(id, by string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.find(id)
	if err != nil {
		return "", err
	}
	if p.Status != StatusPending {
		return "", fmt.Errorf("proposal %s is %s", p.ID, p.Status)
	}
	path, err := s.cfg.Targets().Path(p.Target)
	if err != nil {
		return "", err
	}
	current, exists, err := readTarget(path)
	if err != nil {
		return "", err
	}
	if (p.Base == "") == exists || exists && !sameBase(p.Base, BlobHash([]byte(current))) {
		p.Status, p.Decided, p.DecidedBy = StatusStale, s.cfg.Now().UTC(), by
		p.Reason = p.Target + " changed after it was proposed"
		_ = s.save(p)
		return "", fmt.Errorf("%s changed after proposal %s was written, so it was not applied (it is closed as stale)", p.Target, p.ID)
	}
	if by == "" {
		by = "the operator"
	}
	msg := fmt.Sprintf("memory: %s from proposal %s (session %s), approved by %s\n\n%s", p.Target, p.ID, p.SessionID, by, p.Title)
	if err := tmpl.WriteInstructions(s.cfg.Targets().MemoryDir, path, p.Content, msg); err != nil {
		return "", fmt.Errorf("write %s: %w", p.Target, err)
	}
	p.Previous, p.PrevExisted = current, exists
	p.Status, p.Decided, p.DecidedBy = StatusApproved, s.cfg.Now().UTC(), by
	if err := s.save(p); err != nil {
		slog.Warn("proposals: applied but not recorded", "id", p.ID, "error", err)
	}
	slog.Info("proposals: approved and applied", "id", p.ID, "target", p.Target, "by", by)
	reply := fmt.Sprintf("ok applied proposal %s to %s: %s", p.ID, p.Target, p.Title)
	if SkillName(p.Target) != "" {
		reply += "\n\nThe skill is in every session's workspace from the next session on."
	}
	return reply + fmt.Sprintf("\n\nTake it back with /proposal undo %s.", p.ID), nil
}

// Evaluate starts a comparison of a pending proposal on the eval set: each
// eval is replayed with the current instructions and with the proposal's,
// in the background. The result is posted in the proposal's thread and kept
// with it (/proposal show). It returns at once, saying what it started.
func (s *Service) Evaluate(id string) (string, error) {
	if s.cfg.Eval == nil {
		return "", errors.New("evals are not available in this process")
	}
	s.mu.Lock()
	p, err := s.find(id)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if p.Status != StatusPending {
		return "", fmt.Errorf("proposal %s is %s", p.ID, p.Status)
	}
	n, err := s.cfg.Eval.Check()
	if err != nil {
		return "", err
	}
	go func() {
		ctx := context.Background()
		report, err := s.cfg.Eval.Compare(ctx, map[string]string{p.Target: p.Content})
		if err != nil {
			report = "The comparison failed: " + err.Error()
		}
		s.mu.Lock()
		cur, lerr := s.load(p.ID)
		if lerr == nil {
			cur.Eval, cur.EvalAt = report, s.cfg.Now().UTC()
			_ = s.save(cur)
		}
		s.mu.Unlock()
		msg := fmt.Sprintf("**Eval of proposal %s** (%s): the eval set replayed with the current instructions and with the proposed ones.\n\n%s", p.ID, p.Title, report)
		if where := s.cfg.Where(); where != "" && s.cfg.Chat != nil {
			thread := ""
			if len(p.Threads) > 0 {
				thread = p.Threads[0]
			}
			if err := s.cfg.Chat.PostInThread(ctx, where, thread, msg); err != nil {
				slog.Warn("proposals: could not post the eval", "id", p.ID, "error", err)
			}
		}
		slog.Info("proposals: evaluated", "id", p.ID)
	}()
	return fmt.Sprintf("Replaying %d eval(s) twice, with the current instructions and with proposal %s's, one at a time. The comparison is posted in the proposal's thread and kept in /proposal show %s.", n, p.ID, p.ID), nil
}

// Undo takes an applied proposal back: the target gets its content from
// before (a new file is removed), as an operator edit of its own. It refuses
// when the target changed since.
func (s *Service) Undo(id, by string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.find(id)
	if err != nil {
		return "", err
	}
	if p.Status != StatusApproved {
		return "", fmt.Errorf("proposal %s is %s, not applied", p.ID, p.Status)
	}
	path, err := s.cfg.Targets().Path(p.Target)
	if err != nil {
		return "", err
	}
	current, exists, err := readTarget(path)
	if err != nil {
		return "", err
	}
	if !exists || current != p.Content {
		return "", fmt.Errorf("%s changed after proposal %s was applied: edit it yourself instead", p.Target, p.ID)
	}
	if by == "" {
		by = "the operator"
	}
	mem := s.cfg.Targets().MemoryDir
	msg := fmt.Sprintf("memory: %s back to before proposal %s, undone by %s", p.Target, p.ID, by)
	if p.PrevExisted {
		err = tmpl.WriteInstructions(mem, path, p.Previous, msg)
	} else {
		if err = os.Remove(path); err == nil {
			if SkillName(p.Target) != "" {
				_ = os.Remove(filepath.Dir(path)) // only when empty
			}
			err = tmpl.CommitInstructions(mem, path, msg)
		}
	}
	if err != nil {
		return "", fmt.Errorf("undo %s: %w", p.Target, err)
	}
	p.Status, p.Decided, p.DecidedBy = StatusUndone, s.cfg.Now().UTC(), by
	_ = s.save(p)
	slog.Info("proposals: undone", "id", p.ID, "target", p.Target, "by", by)
	return fmt.Sprintf("ok undid proposal %s: %s is back to what it was before.", p.ID, p.Target), nil
}

// Reject closes a pending proposal. The reason goes into lessons.md so the
// agent does not propose it again.
func (s *Service) Reject(id, by, reason string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.find(id)
	if err != nil {
		return "", err
	}
	if p.Status != StatusPending {
		return "", fmt.Errorf("proposal %s is %s", p.ID, p.Status)
	}
	if by == "" {
		by = "the operator"
	}
	reason = strings.TrimSpace(reason)
	p.Status, p.Decided, p.DecidedBy, p.Reason = StatusRejected, s.cfg.Now().UTC(), by, reason
	if err := s.save(p); err != nil {
		return "", err
	}
	lesson := fmt.Sprintf("- A proposal to change %s was rejected: %q", p.Target, p.Title)
	if reason != "" {
		lesson += ". Reason: " + reason
	}
	lesson += ". Do not propose it again unless something changes."
	if mem := s.cfg.Targets().MemoryDir; mem != "" {
		entry := fmt.Sprintf("### %s\n%s\n", s.cfg.Now().Format("2006-01-02"), lesson)
		if err := tmpl.UpdateMemoryFile(mem, "lessons.md", func(existing string) (string, error) {
			if strings.TrimSpace(existing) == "" {
				return "# Lessons\n\n" + entry, nil
			}
			return strings.TrimRight(existing, "\n") + "\n\n" + entry, nil
		}); err != nil {
			slog.Warn("proposals: could not record the rejection in lessons.md", "id", p.ID, "error", err)
		}
	}
	slog.Info("proposals: rejected", "id", p.ID, "target", p.Target, "by", by)
	return fmt.Sprintf("ok rejected proposal %s (%s). The reason is in lessons.md, so it is not proposed again.", p.ID, p.Title), nil
}

// ThreadReply handles a person's reply in a proposal's thread: yes approves
// it, no (and a reason) rejects it. handled is false for anything else, which
// the thread treats as an ordinary message.
func (s *Service) ThreadReply(thread, by string, admin bool, text string) (reply string, handled bool) {
	if thread == "" {
		return "", false
	}
	p := s.byThread(thread)
	if p == nil {
		return "", false
	}
	verdict, reason := parseVerdict(text)
	if verdict == "" {
		return "", false
	}
	if p.Status != StatusPending {
		return fmt.Sprintf("Proposal %s is already %s.", p.ID, p.Status), true
	}
	if !admin {
		return "Only an admin can approve or reject a proposal.", true
	}
	var err error
	if verdict == "yes" {
		reply, err = s.Approve(p.ID, by)
	} else {
		reply, err = s.Reject(p.ID, by, reason)
	}
	if err != nil {
		return "error: " + err.Error(), true
	}
	return reply, true
}

// parseVerdict reads a reply: "yes" (or approve), or "no" with an optional
// reason after it ("no: too broad", "reject, we lint in CI").
func parseVerdict(text string) (verdict, reason string) {
	t := strings.TrimSpace(text)
	low := strings.ToLower(strings.TrimRight(t, ".!"))
	switch low {
	case "yes", "y", "ok", "approve", "approved", "apply", "lgtm", "yep", "yeah", "sure", "confirm":
		return "yes", ""
	}
	word := low
	if i := strings.IndexAny(low, " ,:;.-—"); i >= 0 {
		word = low[:i]
	}
	switch word {
	case "no", "n", "nope", "reject", "rejected", "nah":
		rest := strings.TrimSpace(t[len(word):])
		return "no", strings.TrimSpace(strings.TrimLeft(rest, ",:;.-— "))
	}
	return "", ""
}

// Sweep reminds once about a proposal pending for 3 days, expires one
// pending for 14, and forgets one decided 90 days ago. It is called
// periodically.
func (s *Service) Sweep(ctx context.Context) {
	now := s.cfg.Now()
	var remind []*Proposal
	s.mu.Lock()
	for _, p := range s.list() {
		if p.Status != StatusPending {
			if !p.Decided.IsZero() && now.Sub(p.Decided) > KeepDecided {
				_ = os.Remove(s.path(p.ID)) // the change itself is in the memory git history
			}
			continue
		}
		age := now.Sub(p.Created)
		switch {
		case age >= ExpireAfter:
			p.Status, p.Decided, p.Reason = StatusExpired, now.UTC(), "not reviewed in 14 days"
			_ = s.save(p)
			slog.Info("proposals: expired", "id", p.ID)
		case age >= RemindAfter && !p.Reminded:
			p.Reminded = true
			_ = s.save(p)
			remind = append(remind, p)
		}
	}
	s.mu.Unlock()
	for _, p := range remind {
		msg := fmt.Sprintf("**Reminder:** proposal %s (%s) has waited %d days for review; it expires after 14.\n\n", p.ID, p.Target, int(now.Sub(p.Created).Hours()/24)) + s.message(p)
		s.post(ctx, p, msg)
	}
}

// Pending is the proposals waiting for review, oldest first.
func (s *Service) Pending() []*Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Proposal
	for _, p := range s.list() {
		if p.Status == StatusPending {
			out = append(out, p)
		}
	}
	return out
}

// Since is every proposal created at or after t, oldest first.
func (s *Service) Since(t time.Time) []*Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Proposal
	for _, p := range s.list() {
		if !p.Created.Before(t) || !p.Decided.IsZero() && !p.Decided.Before(t) {
			out = append(out, p)
		}
	}
	return out
}

// List is /proposals: pending ones first, then the latest decided.
func (s *Service) List() string {
	s.mu.Lock()
	all := s.list()
	s.mu.Unlock()
	var pending, decided []*Proposal
	for _, p := range all {
		if p.Status == StatusPending {
			pending = append(pending, p)
		} else {
			decided = append(decided, p)
		}
	}
	var b strings.Builder
	if len(pending) == 0 {
		b.WriteString("No proposals are waiting for review.\n")
	} else {
		b.WriteString("**Waiting for review** (oldest first):\n")
		for _, p := range pending {
			fmt.Fprintf(&b, "- `%s` %s — %s (%s, %s)\n", p.ID, p.Target, p.Title, p.Created.Local().Format("Jan 2"), p.SessionID)
		}
		b.WriteString("\n/proposal show <id> · /proposal approve <id> · /proposal reject <id> [reason]\n")
	}
	sort.Slice(decided, func(i, j int) bool { return decided[i].Decided.After(decided[j].Decided) })
	if len(decided) > 0 {
		b.WriteString("\n**Recently decided:**\n")
		for _, p := range decided[:min(5, len(decided))] {
			fmt.Fprintf(&b, "- `%s` %s — %s: %s", p.ID, p.Target, p.Title, p.Status)
			if p.Reason != "" {
				fmt.Fprintf(&b, " (%s)", p.Reason)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// Show is /proposal show <id> [full].
func (s *Service) Show(id string, full bool) (string, error) {
	s.mu.Lock()
	p, err := s.find(id)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if p.Status != StatusPending {
		return fmt.Sprintf("**Proposal %s: %s** (%s)\n\nChanges `%s`, from session %s. %s by %s on %s. %s", p.ID, p.Title, p.Status, p.Target, p.SessionID,
			strings.ToUpper(p.Status[:1])+p.Status[1:], orDash(p.DecidedBy), p.Decided.Local().Format("Jan 2 15:04"), p.Reason), nil
	}
	if !full {
		return s.message(p), nil
	}
	return strings.Replace(s.message(p), clip(s.diff(p), maxChatDiff), s.diff(p), 1), nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// find resolves an id or a unique prefix of one. Called under s.mu.
func (s *Service) find(id string) (*Proposal, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("which proposal? (/proposals lists them)")
	}
	if p, err := s.load(id); err == nil {
		return p, nil
	}
	var match []*Proposal
	for _, p := range s.list() {
		if strings.HasPrefix(p.ID, id) || strings.HasPrefix(p.ID, "p-"+id) {
			match = append(match, p)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no proposal %s (/proposals lists them)", id)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("%s matches %d proposals: give more of its id", id, len(match))
}

func (s *Service) byThread(thread string) *Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.list() {
		for _, t := range p.Threads {
			if t == thread {
				return p
			}
		}
	}
	return nil
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "p-" + hex.EncodeToString(b[:])
}

func (s *Service) path(id string) string { return filepath.Join(s.cfg.StateDir, id+".json") }

func (s *Service) load(id string) (*Proposal, error) {
	if strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return nil, errors.New("bad id")
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var p Proposal
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// list is every stored proposal, oldest first. Called under s.mu.
func (s *Service) list() []*Proposal {
	entries, _ := os.ReadDir(s.cfg.StateDir)
	var out []*Proposal
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if p, err := s.load(strings.TrimSuffix(name, ".json")); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *Service) save(p *Proposal) error {
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(p.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(p.ID))
}
