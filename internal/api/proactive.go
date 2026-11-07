package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/proactive"
	"github.com/agent-runner/agent-runner/internal/proposals"
	"github.com/agent-runner/agent-runner/internal/stream"
	"github.com/agent-runner/agent-runner/internal/telegram"
	"github.com/agent-runner/agent-runner/internal/thread"
)

// proactiveChat opens a proactive thread where AGENT_PROACTIVE_CHAT says:
// an agent-stream channel (c_...), or the Telegram chat ("telegram").
type proactiveChat struct {
	stream   *stream.Bot
	telegram *telegram.Bot
}

// chatBot is what a proactive chat needs of a bot.
type chatBot interface {
	OpenProactiveThread(ctx context.Context, target, report, message string, plan bool) error
	StartProactiveRun(ctx context.Context, target, intro, message string) error
	OpenProposalThread(ctx context.Context, target, message string) (string, error)
}

// bot is the bot AGENT_PROACTIVE_CHAT target names.
func (c proactiveChat) bot(target string) (chatBot, error) {
	switch {
	case strings.EqualFold(target, "telegram"):
		if c.telegram == nil {
			return nil, fmt.Errorf("AGENT_PROACTIVE_CHAT is telegram, but the Telegram bot is not configured")
		}
		return c.telegram, nil
	case strings.HasPrefix(target, "c_"):
		if c.stream == nil {
			return nil, fmt.Errorf("AGENT_PROACTIVE_CHAT is an agent-stream channel, but the agent-stream bot is not configured")
		}
		return c.stream, nil
	}
	return nil, fmt.Errorf("AGENT_PROACTIVE_CHAT %q is neither an agent-stream channel id (c_...) nor \"telegram\"", target)
}

func (c proactiveChat) OpenProactiveThread(ctx context.Context, target, report, message string, plan bool) error {
	b, err := c.bot(target)
	if err != nil {
		return err
	}
	return b.OpenProactiveThread(ctx, target, report, message, plan)
}

func (c proactiveChat) StartProactiveRun(ctx context.Context, target, intro, message string) error {
	b, err := c.bot(target)
	if err != nil {
		return err
	}
	return b.StartProactiveRun(ctx, target, intro, message)
}

func (c proactiveChat) OpenProposalThread(ctx context.Context, target, message string) (string, error) {
	b, err := c.bot(target)
	if err != nil {
		return "", err
	}
	return b.OpenProposalThread(ctx, target, message)
}

// newProposals builds the proposals' review (PROACTIVE_DESIGN.md §9).
func newProposals(cfg *config.Config, paths func() (string, string), sb *stream.Bot, tb *telegram.Bot) *proposals.Service {
	return proposals.New(proposals.Config{
		StateDir: filepath.Join(cfg.StateRoot, "proposals"),
		Targets: func() proposals.Targets {
			system, prompt := paths()
			return proposals.Targets{SystemPrompt: system, PromptFile: prompt, MemoryDir: cfg.MemoryDir}
		},
		Chat:    proactiveChat{stream: sb, telegram: tb},
		Where:   func() string { return cfg.Agent.ProactiveChat },
		Enabled: func() bool { return cfg.Agent.Proposals },
	})
}

// proposalLines describes the proposals made or decided since a time, for
// the retrospective.
func proposalLines(p *proposals.Service) func(time.Time) []string {
	return func(since time.Time) []string {
		var out []string
		for _, q := range p.Since(since) {
			line := fmt.Sprintf("- %s `%s` %q (session %s): %s", q.ID, q.Target, q.Title, q.SessionID, q.Status)
			if q.Reason != "" {
				line += " — " + q.Reason
			}
			out = append(out, line)
		}
		return out
	}
}

// sweepProposals reminds about and expires pending proposals, hourly.
func sweepProposals(ctx context.Context, p *proposals.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		p.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newProactive builds the proactive checks (PROACTIVE_DESIGN.md, phase 1).
func newProactive(cfg *config.Config, sessions *agent.Manager, analyzer *thread.Analyzer, sb *stream.Bot, tb *telegram.Bot, props *proposals.Service) *proactive.Service {
	return proactive.New(proactive.Config{
		MemoryDir: cfg.MemoryDir,
		StateDir:  filepath.Join(cfg.StateRoot, "proactive"),
		Settings: func() proactive.Settings {
			a := cfg.Agent
			return proactive.Settings{
				Enabled:    a.Proactive,
				Chat:       a.ProactiveChat,
				Interval:   a.ProactiveInterval,
				MaxPosts:   a.ProactiveMaxPosts,
				MaxTriages: a.ProactiveMaxChecks,

				Retrospective: a.Retrospective,
			}
		},
		Sessions: func() []proactive.Session {
			var out []proactive.Session
			for _, s := range sessions.ListSessions(200) {
				snap := s.Snapshot()
				if snap.CompletedAt == nil {
					continue
				}
				out = append(out, proactive.Session{
					ID: snap.ID, Source: snap.Source, Message: snap.Message, Error: snap.Error,
					Status: string(snap.Status), Ended: *snap.CompletedAt,
				})
			}
			return out
		},
		Triager:   analyzer,
		Chat:      proactiveChat{stream: sb, telegram: tb},
		Proposals: proposalLines(props),
	})
}
