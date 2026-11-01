package chatlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAppendWritesADayFilePerThread(t *testing.T) {
	root := t.TempDir()
	a := &Archive{Root: root}
	day := time.Date(2026, 10, 30, 9, 5, 0, 0, time.Local)
	if err := a.Append("ch_1", "th/1", Entry{Time: day, Speaker: "User", Content: "  deploy the blog  "}); err != nil {
		t.Fatal(err)
	}
	if err := a.Append("ch_1", "th/1", Entry{Time: day.Add(time.Minute), Speaker: "You (the assistant)", Content: "Done."}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(root, "ch_1", "th_1", "2026-10-30.md"))
	want := "# th/1, Friday 30 October 2026\n\n### 09:05 User\n\ndeploy the blog\n\n### 09:06 You (the assistant)\n\nDone.\n\n"
	if got != want {
		t.Errorf("day file:\n%q\nwant\n%q", got, want)
	}
}

func TestAppendWithoutChatIsItsOwnChat(t *testing.T) {
	root := t.TempDir()
	a := &Archive{Root: root}
	if err := a.Append("", "12345", Entry{Speaker: "User", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := ChatOf(root, "12345"); got != "12345" {
		t.Errorf("ChatOf = %q, want the thread itself", got)
	}
}

func TestSweepDropsOnlyDaysPastRetention(t *testing.T) {
	root := t.TempDir()
	a := &Archive{Root: root, Retention: 90 * 24 * time.Hour}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	old, kept := now.AddDate(0, 0, -91), now.AddDate(0, 0, -89)
	for _, e := range []struct {
		chat string
		at   time.Time
	}{{"a", old}, {"a", kept}, {"b", old}} {
		if err := a.Append(e.chat, "t-"+e.chat, Entry{Time: e.at, Speaker: "User", Content: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	a.Sweep(now)
	if _, err := os.Stat(filepath.Join(root, "a", "t-a", old.Format(dayLayout)+".md")); !os.IsNotExist(err) {
		t.Errorf("a day past retention was kept (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "t-a", kept.Format(dayLayout)+".md")); err != nil {
		t.Errorf("a day within retention was dropped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b")); !os.IsNotExist(err) {
		t.Errorf("a chat left with nothing was kept (err %v)", err)
	}
}

func TestCopyChatCopiesOnlyTheThreadsChat(t *testing.T) {
	root := t.TempDir()
	a := &Archive{Root: root}
	for _, e := range []struct{ chat, thread, text string }{
		{"chan-a", "t1", "the database password rotation"},
		{"chan-a", "t2", "current request"},
		{"chan-b", "t3", "someone else's chat"},
	} {
		if err := a.Append(e.chat, e.thread, Entry{Speaker: "User", Content: e.text}); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(filepath.Join(dst, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := CopyChat(root, "t2", dst)
	if err != nil {
		t.Fatal(err)
	}
	if name != "t2" {
		t.Errorf("thread folder = %q, want t2", name)
	}
	entries, _ := os.ReadDir(dst)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != "t1,t2" {
		t.Errorf("copied threads = %v, want [t1 t2] (no other chat, no stale copy)", got)
	}
	day := time.Now().Format(dayLayout) + ".md"
	if !strings.Contains(readFile(t, filepath.Join(dst, "t1", day)), "password rotation") {
		t.Error("an earlier thread of the chat was not copied")
	}
}

func TestCopyChatWithoutRecordLeavesNothing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := CopyChat(t.TempDir(), "nobody", dst)
	if err != nil || name != "" {
		t.Fatalf("CopyChat = %q, %v; want \"\", nil", name, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("an earlier copy was left behind (err %v)", err)
	}
}

func TestChatOfPrefersTheChatOverTheThreadAlone(t *testing.T) {
	root := t.TempDir()
	a := &Archive{Root: root}
	// A reply recorded before the thread's channel was known, then the
	// thread's messages in its channel.
	if err := a.Append("", "t1", Entry{Speaker: "User", Content: "early"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Append("chan", "t1", Entry{Speaker: "User", Content: "later"}); err != nil {
		t.Fatal(err)
	}
	if got := ChatOf(root, "t1"); got != "chan" {
		t.Errorf("ChatOf = %q, want chan", got)
	}
}
