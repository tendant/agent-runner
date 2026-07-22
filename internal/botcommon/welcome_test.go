package botcommon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingSender captures Final sends.
type recordingSender struct {
	finals []string
}

func (s *recordingSender) Status(_ context.Context, _, text string) {}
func (s *recordingSender) Reply(_ context.Context, _, text string)  {}
func (s *recordingSender) Final(_ context.Context, _, text string) {
	s.finals = append(s.finals, text)
}

func welcomeEngine(t *testing.T, enabled bool) (*Engine, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	e := &Engine{
		Sender: sender,
		Label:  "test",
		Welcome: Welcome{
			Enabled:  enabled,
			Text:     "hello there",
			StateDir: filepath.Join(t.TempDir(), "welcomed"),
		},
	}
	return e, sender
}

func TestWelcomeIfNeeded_FirstContactOnly(t *testing.T) {
	e, sender := welcomeEngine(t, true)

	e.WelcomeIfNeeded(context.Background(), "conv-1")
	e.WelcomeIfNeeded(context.Background(), "conv-1")
	e.WelcomeIfNeeded(context.Background(), "conv-1")

	if len(sender.finals) != 1 {
		t.Fatalf("expected exactly 1 welcome, got %d", len(sender.finals))
	}
	if sender.finals[0] != "hello there" {
		t.Errorf("unexpected welcome text: %q", sender.finals[0])
	}
}

func TestWelcomeIfNeeded_PerConversation(t *testing.T) {
	e, sender := welcomeEngine(t, true)

	e.WelcomeIfNeeded(context.Background(), "conv-a")
	e.WelcomeIfNeeded(context.Background(), "conv-b")

	if len(sender.finals) != 2 {
		t.Errorf("expected one welcome per conversation, got %d", len(sender.finals))
	}
}

func TestWelcomeIfNeeded_Disabled(t *testing.T) {
	e, sender := welcomeEngine(t, false)

	e.WelcomeIfNeeded(context.Background(), "conv-1")

	if len(sender.finals) != 0 {
		t.Errorf("expected no welcome when disabled, got %d", len(sender.finals))
	}
}

func TestWelcomeIfNeeded_UnsafeIDSanitized(t *testing.T) {
	e, sender := welcomeEngine(t, true)

	e.WelcomeIfNeeded(context.Background(), "../../etc/passwd")
	e.WelcomeIfNeeded(context.Background(), "../../etc/passwd")

	if len(sender.finals) != 1 {
		t.Fatalf("expected 1 welcome for unsafe id, got %d", len(sender.finals))
	}
	// The marker must be inside StateDir, not escaped via traversal.
	entries, err := os.ReadDir(e.Welcome.StateDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 marker inside state dir, got %v (err %v)", len(entries), err)
	}
	if strings.Contains(entries[0].Name(), "/") {
		t.Errorf("marker name contains a path separator: %q", entries[0].Name())
	}
}

func TestLoadWelcomeText_DefaultAndOverride(t *testing.T) {
	if got := LoadWelcomeText(""); got != DefaultWelcomeText {
		t.Error("empty memory dir should return the default")
	}

	dir := t.TempDir()
	if got := LoadWelcomeText(dir); got != DefaultWelcomeText {
		t.Error("missing WELCOME.md should return the default")
	}

	os.WriteFile(filepath.Join(dir, "WELCOME.md"), []byte("custom greeting\n"), 0644)
	if got := LoadWelcomeText(dir); got != "custom greeting" {
		t.Errorf("expected override, got %q", got)
	}

	os.WriteFile(filepath.Join(dir, "WELCOME.md"), []byte("   \n"), 0644)
	if got := LoadWelcomeText(dir); got != DefaultWelcomeText {
		t.Error("blank WELCOME.md should fall back to the default")
	}
}
