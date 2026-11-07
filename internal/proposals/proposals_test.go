package proposals

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBlobHashMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := filepath.Join(t.TempDir(), "x")
	content := "line one\nline two\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "hash-object", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := BlobHash([]byte(content)); got != strings.TrimSpace(string(out)) {
		t.Fatalf("BlobHash %s, git %s", got, out)
	}
}

func TestParse(t *testing.T) {
	p, err := Parse("---\ntarget: prompt.md\nbase: 3F9C2A1  # what I read\ntitle: \"Lint first\"\nevidence: [sess-1, 'sess-2']\n---\n\n## Why\nThree sessions failed gofmt.\n\n## Change\n\n```markdown\n# Workflow\n\n## Steps\nRun gofmt -l before committing.\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.Target != "prompt.md" || p.Base != "3f9c2a1" || p.Title != "Lint first" || len(p.Evidence) != 2 || p.Evidence[1] != "sess-2" {
		t.Fatalf("%+v", p)
	}
	if p.Why != "Three sessions failed gofmt." {
		t.Fatalf("why %q", p.Why)
	}
	if p.Content != "# Workflow\n\n## Steps\nRun gofmt -l before committing.\n" {
		t.Fatalf("content %q", p.Content)
	}

	for name, bad := range map[string]string{
		"no front matter": "## Change\nx\n",
		"no target":       "---\ntitle: t\n---\n## Change\nx\n",
		"no change":       "---\ntarget: prompt.md\ntitle: t\n---\n## Why\nx\n",
		"a diff":          "---\ntarget: prompt.md\ntitle: t\n---\n## Change\n--- a/prompt.md\n+++ b/prompt.md\n@@ -1 +1 @@\n-a\n+b\n",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestTargets(t *testing.T) {
	tg := Targets{SystemPrompt: "/m/agent.md", PromptFile: "/m/prompt.md", MemoryDir: "/m"}
	for target, ok := range map[string]bool{
		"agent.md": true, "prompt.md": true, "GOALS.md": true, "skills/release-notes/SKILL.md": true,
		"PROACTIVE.md": false, "ALERTS.md": false, "lessons.md": false, "skills/../agent.md": false,
		"skills/x/other.md": false, "skills/Bad/SKILL.md": false, "../.env": false, ".env.local": false,
	} {
		if _, err := tg.Path(target); (err == nil) != ok {
			t.Errorf("%s: allowed=%v, want %v (%v)", target, err == nil, ok, err)
		}
	}
}

func TestDiff(t *testing.T) {
	d := Diff("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\nk\n", "x.md")
	want := "--- a/x.md\n+++ b/x.md\n@@ -2,9 +2,10 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n i\n j\n+k\n"
	if d != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", d, want)
	}
	if d := Diff("", "new\n", "n.md"); !strings.Contains(d, "@@ -0,0 +1,1 @@\n+new\n") {
		t.Fatalf("new file diff:\n%s", d)
	}
}

type fakeChat struct {
	posts []string
	n     int
}

func (c *fakeChat) OpenProposalThread(_ context.Context, _, message string) (string, error) {
	c.posts = append(c.posts, message)
	c.n++
	return "thread-" + string(rune('0'+c.n)), nil
}

type fixture struct {
	svc  *Service
	mem  string
	chat *fakeChat
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{mem: t.TempDir(), chat: &fakeChat{}, now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	f.svc = New(Config{
		StateDir: filepath.Join(t.TempDir(), "proposals"),
		Targets: func() Targets {
			return Targets{SystemPrompt: filepath.Join(f.mem, "agent.md"), PromptFile: filepath.Join(f.mem, "prompt.md"), MemoryDir: f.mem}
		},
		Chat:  f.chat,
		Where: func() string { return "c_1" },
		Now:   func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(f.mem, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) read(t *testing.T, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(f.mem, name))
	return string(data)
}

func proposal(target, base, title, content string) File {
	head := "---\ntarget: " + target + "\ntitle: " + title + "\n"
	if base != "" {
		head += "base: " + base + "\n"
	}
	return File{Name: title + ".md", Data: []byte(head + "---\n## Why\nbecause\n\n## Change\n" + content)}
}

func TestSubmitApprove(t *testing.T) {
	f := newFixture(t)
	f.write(t, "prompt.md", "# Workflow\nold step\n")
	base := BlobHash([]byte("# Workflow\nold step\n"))[:7]

	notes := f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: []File{proposal("prompt.md", base, "Lint first", "# Workflow\nlint, then commit\n")}})
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "posted for review in c_1") {
		t.Fatalf("notes %v", notes)
	}
	if len(f.chat.posts) != 1 || !strings.Contains(f.chat.posts[0], "-old step\n+lint, then commit") || !strings.Contains(f.chat.posts[0], "Reply **yes**") {
		t.Fatalf("post:\n%v", f.chat.posts)
	}
	pending := f.svc.Pending()
	if len(pending) != 1 || pending[0].Threads[0] != "thread-1" {
		t.Fatalf("pending %+v", pending)
	}

	// A no-reply in an unrelated thread, or chatter in this one, is not a verdict.
	if _, ok := f.svc.ThreadReply("other", "u", true, "yes"); ok {
		t.Fatal("handled a reply in another thread")
	}
	if _, ok := f.svc.ThreadReply("thread-1", "u", true, "why lint?"); ok {
		t.Fatal("handled a question as a verdict")
	}
	if r, ok := f.svc.ThreadReply("thread-1", "u", false, "yes"); !ok || !strings.Contains(r, "Only an admin") {
		t.Fatalf("non-admin: %q %v", r, ok)
	}
	r, ok := f.svc.ThreadReply("thread-1", "alice", true, "yes")
	if !ok || !strings.HasPrefix(r, "ok applied") {
		t.Fatalf("approve: %q", r)
	}
	if got := f.read(t, "prompt.md"); got != "# Workflow\nlint, then commit\n" {
		t.Fatalf("prompt.md %q", got)
	}
	if r, _ := f.svc.ThreadReply("thread-1", "alice", true, "no"); !strings.Contains(r, "already approved") {
		t.Fatalf("second verdict: %q", r)
	}
	if _, err := f.svc.Undo(pending[0].ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "prompt.md"); got != "# Workflow\nold step\n" {
		t.Fatalf("after undo prompt.md %q", got)
	}
	if _, err := f.svc.Undo(pending[0].ID, "alice"); err == nil {
		t.Fatal("undid twice")
	}
}

func TestApproveRefusesWhenTargetChanged(t *testing.T) {
	f := newFixture(t)
	f.write(t, "GOALS.md", "Keep CI green.\n")
	f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: []File{proposal("GOALS.md", "", "Watch deploys", "Keep CI green.\nWatch deploys.\n")}})
	id := f.svc.Pending()[0].ID
	f.write(t, "GOALS.md", "Keep CI green, always.\n") // the operator edited it meanwhile
	if _, err := f.svc.Approve(id, "alice"); err == nil || !strings.Contains(err.Error(), "changed after") {
		t.Fatalf("approved over an edit: %v", err)
	}
	if got := f.read(t, "GOALS.md"); got != "Keep CI green, always.\n" {
		t.Fatalf("GOALS.md %q", got)
	}
	if len(f.svc.Pending()) != 0 {
		t.Fatal("a stale proposal is still pending")
	}
}

func TestSubmitDrops(t *testing.T) {
	f := newFixture(t)
	f.write(t, "prompt.md", "current\n")
	secret := func(s string) bool { return strings.Contains(s, "hunter2") }
	files := []File{
		proposal("PROACTIVE.md", "", "Raise caps", "max_posts_per_day: 999\n"),
		proposal("prompt.md", "deadbeef", "Stale base", "new\n"),
		proposal("prompt.md", "", "No change", "current\n"),
		proposal("prompt.md", "", "Leak", "token hunter2\n"),
		{Name: "junk.md", Data: []byte("not a proposal")},
	}
	notes := f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: files, Secret: secret})
	if len(notes) != len(files) {
		t.Fatalf("notes %v", notes)
	}
	for i, want := range []string{"cannot be proposed", "no longer matches", "no change", "secret", "front matter"} {
		if !notes[i].Dropped || !strings.Contains(notes[i].Text, want) {
			t.Errorf("note %d %+v lacks %q", i, notes[i], want)
		}
	}
	if len(f.svc.Pending()) != 0 || len(f.chat.posts) != 0 {
		t.Fatal("something invalid was stored or posted")
	}
}

func TestSubmitLimits(t *testing.T) {
	f := newFixture(t)
	var files []File
	for i := range 5 {
		files = append(files, proposal("skills/s"+string(rune('a'+i))+"/SKILL.md", "", "skill "+string(rune('a'+i)), "# skill\n"))
	}
	notes := f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: files})
	if n := len(f.svc.Pending()); n != MaxPerSession {
		t.Fatalf("%d stored, want %d: %v", n, MaxPerSession, notes)
	}
	// Per target: three waiting for prompt.md, the fourth is refused.
	for i := range 4 {
		notes = f.svc.Submit(context.Background(), Submission{SessionID: "s" + string(rune('2'+i)), Files: []File{proposal("prompt.md", "", "p", "v"+string(rune('0'+i))+"\n")}})
	}
	if !strings.Contains(notes[0].Text, "already waiting") {
		t.Fatalf("fourth for one target: %v", notes)
	}
}

func TestRejectRecordsLesson(t *testing.T) {
	f := newFixture(t)
	f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: []File{proposal("skills/deploy/SKILL.md", "", "Deploy skill", "# Deploy\n")}})
	r, ok := f.svc.ThreadReply("thread-1", "alice", true, "no: we deploy by hand")
	if !ok || !strings.HasPrefix(r, "ok rejected") {
		t.Fatalf("reject: %q", r)
	}
	lessons := f.read(t, "lessons.md")
	if !strings.Contains(lessons, `"Deploy skill"`) || !strings.Contains(lessons, "Reason: we deploy by hand") {
		t.Fatalf("lessons.md:\n%s", lessons)
	}
	if _, err := os.Stat(filepath.Join(f.mem, "skills", "deploy")); !os.IsNotExist(err) {
		t.Fatal("a rejected skill was written")
	}
}

func TestApproveNewSkill(t *testing.T) {
	f := newFixture(t)
	f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: []File{proposal("skills/release-notes/SKILL.md", "", "Release notes skill", "---\nname: release-notes\n---\nWrite them.\n")}})
	if _, err := f.svc.Approve(f.svc.Pending()[0].ID[:5], "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, "skills/release-notes/SKILL.md"); !strings.Contains(got, "Write them.") {
		t.Fatalf("skill %q", got)
	}
	id := f.svc.Since(time.Time{})[0].ID
	f.write(t, "skills/release-notes/SKILL.md", "edited by hand\n")
	if _, err := f.svc.Undo(id, "alice"); err == nil {
		t.Fatal("undid over a hand edit")
	}
	f.write(t, "skills/release-notes/SKILL.md", "---\nname: release-notes\n---\nWrite them.\n")
	if _, err := f.svc.Undo(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.mem, "skills", "release-notes")); !os.IsNotExist(err) {
		t.Fatal("undoing a new skill left it in place")
	}
}

func TestSweep(t *testing.T) {
	f := newFixture(t)
	f.svc.Submit(context.Background(), Submission{SessionID: "s1", Files: []File{proposal("GOALS.md", "", "g", "goal\n")}})
	f.now = f.now.Add(RemindAfter + time.Hour)
	f.svc.Sweep(context.Background())
	f.svc.Sweep(context.Background())
	if len(f.chat.posts) != 2 || !strings.HasPrefix(f.chat.posts[1], "**Reminder:**") {
		t.Fatalf("posts %d: %v", len(f.chat.posts), f.chat.posts)
	}
	// Yes in the reminder's thread counts too.
	if p := f.svc.Pending()[0]; len(p.Threads) != 2 {
		t.Fatalf("threads %v", p.Threads)
	}
	f.now = f.now.Add(ExpireAfter)
	f.svc.Sweep(context.Background())
	if len(f.svc.Pending()) != 0 || !strings.Contains(f.svc.List(), "expired") {
		t.Fatalf("not expired:\n%s", f.svc.List())
	}
	f.now = f.now.Add(KeepDecided + time.Hour)
	f.svc.Sweep(context.Background())
	if strings.Contains(f.svc.List(), "expired") {
		t.Fatalf("an old decided proposal was kept:\n%s", f.svc.List())
	}
}

func TestParseVerdict(t *testing.T) {
	for text, want := range map[string][2]string{
		"yes":                   {"yes", ""},
		"LGTM!":                 {"yes", ""},
		"no":                    {"no", ""},
		"No, we lint in CI":     {"no", "we lint in CI"},
		"reject: too broad":     {"no", "too broad"},
		"not sure, why?":        {"", ""},
		"nothing to add":        {"", ""},
		"yes but change step 2": {"", ""},
	} {
		v, r := parseVerdict(text)
		if v != want[0] || r != want[1] {
			t.Errorf("%q: %q %q, want %q %q", text, v, r, want[0], want[1])
		}
	}
}
