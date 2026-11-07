package chatcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/proactive"
)

type fakeProactive struct{ forced bool }

func (f *fakeProactive) Status() string { return "STATUS" }
func (f *fakeProactive) Agenda() string { return "AGENDA" }
func (f *fakeProactive) Tick(_ context.Context, force bool) proactive.Outcome {
	f.forced = force
	return proactive.Outcome{Result: "plan", Items: []string{"agenda:x"}, Message: "m"}
}

func (f *fakeProactive) RetroStatus() string { return "RETRO" }
func (f *fakeProactive) TrustStatus() string { return "TRUST" }
func (f *fakeProactive) SetTrust(class, level string) (string, error) {
	return "set " + class + " " + level, nil
}
func (f *fakeProactive) SessionUndone(context.Context, string) string { return "" }
func (f *fakeProactive) Retrospective(context.Context) proactive.Outcome {
	return proactive.Outcome{Result: "started"}
}

func TestProactiveCommands(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	if got, _, ok := c.Handle("/proactive", nil); !ok || !strings.Contains(got, "not available") {
		t.Fatalf("without checks: %q %v", got, ok)
	}
	p := &fakeProactive{}
	c.SetProactive(p)
	c.cfg.Agent.ProactiveChat = "c_1"
	if got, _, _ := c.Handle("/proactive", nil); got != "STATUS" {
		t.Fatalf("status: %q", got)
	}
	if got, _, _ := c.Handle("/agenda", nil); got != "AGENDA" {
		t.Fatalf("agenda: %q", got)
	}
	if got, _, _ := c.Handle("/proactive tick", nil); !p.forced || !strings.Contains(got, "posted in c_1") {
		t.Fatalf("tick: %q forced=%v", got, p.forced)
	}

	if got, _, _ := c.Handle("/goals set Keep CI green on main.", nil); !strings.HasPrefix(got, "ok") {
		t.Fatalf("goals set: %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(c.cfg.MemoryDir, proactive.GoalsFile))
	if strings.TrimSpace(string(data)) != "Keep CI green on main." {
		t.Fatalf("GOALS.md: %q", data)
	}
	if got, _, _ := c.Handle("/goals", nil); !strings.Contains(got, "Keep CI green") {
		t.Fatalf("goals show: %q", got)
	}
	got, _, _ := c.Handle("/proactive policy set ---\ninterval: soon\n---\nPropose fixes.", nil)
	if !strings.HasPrefix(got, "ok") || !strings.Contains(got, "interval") {
		t.Fatalf("policy set should warn about the bad interval: %q", got)
	}
	if got, _, _ := c.Handle("/goals clear", nil); !strings.HasPrefix(got, "ok") {
		t.Fatalf("goals clear: %q", got)
	}
}

// /proactive on|off is /config set AGENT_PROACTIVE, saved to .env.local.
func TestProactiveOnOff(t *testing.T) {
	env := setupTestEnv(t)
	c := NewCommander(env.cfg, env.rt)
	got, _, _ := c.Handle("/proactive on", nil)
	if !env.cfg.Agent.Proactive || !strings.Contains(got, "AGENT_PROACTIVE_CHAT") {
		t.Fatalf("on: %q (enabled %v)", got, env.cfg.Agent.Proactive)
	}
	if c.Handle("/proactive off", nil); env.cfg.Agent.Proactive {
		t.Fatal("off did not turn the checks off")
	}
}
