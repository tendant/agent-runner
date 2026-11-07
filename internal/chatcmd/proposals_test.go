package chatcmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
)

type fakeProposals struct{ calls []string }

func (f *fakeProposals) List() string { return "LIST" }
func (f *fakeProposals) Show(id string, full bool) (string, error) {
	f.calls = append(f.calls, "show "+id)
	if full {
		f.calls = append(f.calls, "full")
	}
	return "SHOW", nil
}
func (f *fakeProposals) Approve(id, by string) (string, error) {
	f.calls = append(f.calls, "approve "+id)
	if id == "" {
		return "", errors.New("which proposal?")
	}
	return "ok applied", nil
}
func (f *fakeProposals) Reject(id, by, reason string) (string, error) {
	f.calls = append(f.calls, "reject "+id+" "+reason)
	return "ok rejected", nil
}

func (f *fakeProposals) Evaluate(id string) (string, error) {
	f.calls = append(f.calls, "eval "+id)
	return "started", nil
}

func (f *fakeProposals) Undo(id, by string) (string, error) {
	f.calls = append(f.calls, "undo "+id)
	return "ok undid", nil
}

func TestProposalCommands(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	if got, _, _ := c.Handle("/proposals", nil); !strings.Contains(got, "not available") {
		t.Fatalf("without proposals: %q", got)
	}
	p := &fakeProposals{}
	c.SetProposals(p)
	c.cfg.Agent.Proposals = true
	if got, _, _ := c.Handle("/proposals", nil); got != "LIST" {
		t.Fatalf("list: %q", got)
	}
	c.Handle("/proposal show p-1 full", nil)
	c.Handle("/proposal approve p-1", nil)
	c.Handle("/proposal reject p-2 we lint in CI", nil)
	c.Handle("/proposal undo p-1", nil)
	if got := strings.Join(p.calls, "|"); got != "show p-1|full|approve p-1|reject p-2 we lint in CI|undo p-1" {
		t.Fatalf("calls %s", got)
	}
	if got, _, _ := c.Handle("/proposal approve", nil); !strings.HasPrefix(got, "error: which") {
		t.Fatalf("no id: %q", got)
	}
	if got, _, _ := c.Handle("/proposal", nil); !strings.Contains(got, "/proposal approve <id>") {
		t.Fatalf("help: %q", got)
	}
	c.cfg.Agent.Proposals = false
	if got, _, _ := c.Handle("/proposals", nil); !strings.Contains(got, "AGENT_PROPOSALS is off") {
		t.Fatalf("off: %q", got)
	}
}

func TestRetrospectiveCommand(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	c.SetProactive(&fakeProactive{})
	c.cfg.Agent.ProactiveChat = "c_1"
	if got, _, _ := c.Handle("/retrospective", nil); got != "RETRO" {
		t.Fatalf("status: %q", got)
	}
	if got, _, _ := c.Handle("/retrospective now", nil); !strings.Contains(got, "started in c_1") {
		t.Fatalf("now: %q", got)
	}
}

func TestTrustCommand(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	if got, _, _ := c.Handle("/trust", nil); !strings.Contains(got, "not available") {
		t.Fatalf("without checks: %q", got)
	}
	c.SetProactive(&fakeProactive{})
	if got, _, _ := c.Handle("/trust", nil); got != "TRUST" {
		t.Fatalf("status: %q", got)
	}
	if got, _, _ := c.Handle("/trust ci-fix notify", nil); got != "set ci-fix notify" {
		t.Fatalf("set: %q", got)
	}
	if got, _, _ := c.Handle("/trust ci-fix", nil); !strings.HasPrefix(got, "error: usage") {
		t.Fatalf("one arg: %q", got)
	}
}

func TestEvalsCommand(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	if got, _, _ := c.Handle("/evals", nil); !strings.Contains(got, "empty") {
		t.Fatalf("empty: %q", got)
	}
	if got, _, _ := c.Handle("/evals set lint\n---\nreply: gofmt\n---\nAdd hello.go.", nil); got != "ok saved eval lint (1 check(s))" {
		t.Fatalf("set (newline): %q", got)
	}
	if got, _, _ := c.Handle("/evals set vet ---\ncommand: go vet ./...\n---\nFix the vet error.", nil); got != "ok saved eval vet (1 check(s))" {
		t.Fatalf("set (space): %q", got)
	}
	if got, _, _ := c.Handle("/evals", nil); !strings.Contains(got, "**lint**: 1 check(s) · Add hello.go.") || !strings.Contains(got, "**vet**") {
		t.Fatalf("list: %q", got)
	}
	if got, _, _ := c.Handle("/evals show vet", nil); !strings.Contains(got, "command: go vet") {
		t.Fatalf("show: %q", got)
	}
	if got, _, _ := c.Handle("/evals set broken no checks here", nil); !strings.HasPrefix(got, "error:") {
		t.Fatalf("bad eval: %q", got)
	}
	if got, _, _ := c.Handle("/evals remove lint", nil); got != "ok removed eval lint" {
		t.Fatalf("remove: %q", got)
	}
	p := &fakeProposals{}
	c.SetProposals(p)
	if got, _, _ := c.Handle("/proposal eval p-1", nil); got != "started" || p.calls[0] != "eval p-1" {
		t.Fatalf("eval: %q %v", got, p.calls)
	}
}
