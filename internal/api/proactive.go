package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/evals"
	"github.com/agent-runner/agent-runner/internal/proactive"
	"github.com/agent-runner/agent-runner/internal/proposals"
	"github.com/agent-runner/agent-runner/internal/repos"
	"github.com/agent-runner/agent-runner/internal/stream"
	"github.com/agent-runner/agent-runner/internal/subagent"
	"github.com/agent-runner/agent-runner/internal/telegram"
	"github.com/agent-runner/agent-runner/internal/thread"
	"github.com/agent-runner/agent-runner/internal/trust"
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
	StartProactiveRun(ctx context.Context, target, intro, message string) (string, string, error)
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

func (c proactiveChat) StartProactiveRun(ctx context.Context, target, intro, message string) (string, string, error) {
	b, err := c.bot(target)
	if err != nil {
		return "", "", err
	}
	return b.StartProactiveRun(ctx, target, intro, message)
}

// PostInThread posts in a proposal's thread (agent-stream), or in the
// Telegram chat, which has no threads.
func (c proactiveChat) PostInThread(ctx context.Context, target, thread, message string) error {
	switch {
	case strings.HasPrefix(target, "c_") && c.stream != nil && thread != "":
		c.stream.NotifyConversation(ctx, thread, message)
		return nil
	case strings.HasPrefix(target, "c_") && c.stream != nil:
		_, err := c.stream.OpenProposalThread(ctx, target, message)
		return err
	case strings.EqualFold(target, "telegram") && c.telegram != nil:
		_, err := c.telegram.OpenProposalThread(ctx, target, message)
		return err
	}
	return fmt.Errorf("AGENT_PROACTIVE_CHAT %q: no bot to post in", target)
}

func (c proactiveChat) OpenProposalThread(ctx context.Context, target, message string) (string, error) {
	b, err := c.bot(target)
	if err != nil {
		return "", err
	}
	return b.OpenProposalThread(ctx, target, message)
}

// evaluator is the eval set's comparison (PROACTIVE_DESIGN.md §11) as the
// proposals see it.
type evaluator struct{ r *evals.Runner }

func (e evaluator) Check() (int, error) { return e.r.Check() }
func (e evaluator) Compare(ctx context.Context, overrides map[string]string) (string, error) {
	rep, err := e.r.Compare(ctx, overrides)
	if err != nil {
		return "", err
	}
	return rep.String(), nil
}

// newEvals builds the eval runner. Replays run sandboxed: AGENT_SANDBOX
// must not be off. budgetLeft and spend are wired once the proactive
// checks exist.
func newEvals(cfg *config.Config, h *Handlers) *evals.Runner {
	return &evals.Runner{
		MemoryDir: cfg.MemoryDir,
		Start:     h.execEngine.StartEval,
		Get:       h.execEngine.GetAgentSession,
		Ready: func() error {
			if strings.EqualFold(cfg.Agent.SandboxMode, "off") || cfg.Agent.SandboxMode == "" {
				return fmt.Errorf("eval replays run sandboxed, and AGENT_SANDBOX is off")
			}
			return nil
		},
	}
}

// newProposals builds the proposals' review (PROACTIVE_DESIGN.md §9).
func newProposals(cfg *config.Config, paths func() (string, string), sb *stream.Bot, tb *telegram.Bot, ev *evals.Runner) *proposals.Service {
	return proposals.New(proposals.Config{
		Eval:     evaluator{ev},
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
// quietRunner starts a quiet proactive run: an ordinary session of source
// "proactive", which no chat watches and the notifier skips.
type quietRunner struct {
	start func(message, source, convID string) (string, error)
}

func (r quietRunner) StartQuietRun(message string) (string, error) {
	return r.start(message, "proactive", "")
}

func newProactive(cfg *config.Config, sessions *agent.Manager, analyzer *thread.Analyzer, sb *stream.Bot, tb *telegram.Bot, props *proposals.Service, start func(message, source, convID string) (string, error)) *proactive.Service {
	probes := &proactive.RepoSignals{
		Repos:     repos.New(cfg.ReposFile).List,
		CacheRoot: cfg.RepoCacheRoot,
		Secrets:   agentsecrets.New(cfg.SecretsFile),
	}
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
				MaxSessions:   a.ProactiveMaxRuns,
				BudgetUSD:     a.ProactiveBudgetUSD,
				Sandboxed:     strings.EqualFold(a.SandboxMode, "strict"),
			}
		},
		Sessions: func() []proactive.Session {
			var out []proactive.Session
			for _, s := range sessions.ListSessions(200) {
				snap := s.Snapshot()
				if snap.CompletedAt == nil {
					continue
				}
				ps := proactive.Session{
					ID: snap.ID, Source: snap.Source, Message: snap.Message, Error: snap.Error,
					Status: string(snap.Status), Ended: *snap.CompletedAt, CostUSD: snap.TotalCostUSD,
				}
				if rr, ok := snap.ReviewJSON.(*subagent.ReviewResult); ok && rr != nil {
					ps.ReviewScore = rr.Score
				}
				for _, c := range snap.Pushed {
					ps.Pushed = append(ps.Pushed, proactive.Commit{Repo: c.Repo, SHA: c.SHA, Subject: c.Subject})
				}
				out = append(out, ps)
			}
			return out
		},
		Triager:   analyzer,
		Chat:      proactiveChat{stream: sb, telegram: tb},
		Proposals: proposalLines(props),
		Trust:     trust.New(filepath.Join(cfg.StateRoot, "trust.json")),
		Runner:    quietRunner{start: start},
		Signals:   probes,
		Reverts:   probes,
		Judge:     analyzer,
	})
}
