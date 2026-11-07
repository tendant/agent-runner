package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/proactive"
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

func (c proactiveChat) OpenProactiveThread(ctx context.Context, target, report, message string, plan bool) error {
	switch {
	case strings.EqualFold(target, "telegram"):
		if c.telegram == nil {
			return fmt.Errorf("AGENT_PROACTIVE_CHAT is telegram, but the Telegram bot is not configured")
		}
		return c.telegram.OpenProactiveThread(ctx, target, report, message, plan)
	case strings.HasPrefix(target, "c_"):
		if c.stream == nil {
			return fmt.Errorf("AGENT_PROACTIVE_CHAT is an agent-stream channel, but the agent-stream bot is not configured")
		}
		return c.stream.OpenProactiveThread(ctx, target, report, message, plan)
	}
	return fmt.Errorf("AGENT_PROACTIVE_CHAT %q is neither an agent-stream channel id (c_...) nor \"telegram\"", target)
}

// newProactive builds the proactive checks (PROACTIVE_DESIGN.md, phase 1).
func newProactive(cfg *config.Config, sessions *agent.Manager, analyzer *thread.Analyzer, sb *stream.Bot, tb *telegram.Bot) *proactive.Service {
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
		Triager: analyzer,
		Chat:    proactiveChat{stream: sb, telegram: tb},
	})
}
