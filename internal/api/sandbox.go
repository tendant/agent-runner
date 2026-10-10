package api

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agenthome"
	"github.com/agent-runner/agent-runner/internal/clisetup"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/llm"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/codexlogin"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

// newSandboxRuntime builds the sandbox runtime from config. An invalid
// configuration yields a runtime that rejects every run rather than silently
// running agents on the host.
func newSandboxRuntime(cfg *config.Config) *sandboxrt.Runtime {
	mode := sandbox.Mode(cfg.Agent.SandboxMode)
	if mode == "" || mode == sandbox.ModeOff {
		return nil
	}
	host, _ := os.Hostname()
	rt, err := sandboxrt.New(sandboxrt.Config{
		Mode:          mode,
		StateDir:      filepath.Join(cfg.StateRoot, "sandbox"),
		Instance:      host,
		IsoboxBin:     cfg.Agent.SandboxIsobox,
		IsoboxBackend: cfg.Agent.SandboxBackend,
		PolicyFile:    cfg.Agent.SandboxPolicyFile,
		Memory:        cfg.Agent.SandboxMemory,
		PIDs:          cfg.Agent.SandboxPIDs,
		EvidenceFile:  cfg.Agent.SandboxEvidence,
		EnvAllow:      cfg.Agent.SandboxEnvAllow,
		PrivatePaths:  sandboxPrivatePaths(cfg),
		Claude:        sandboxClaudeSeed(cfg),
		Models:        sandboxModelChannels(cfg),
		CodexSeed:     sandboxSeedDir(cfg, ".codex", "codex"),
		PiSeed:        sandboxSeedDir(cfg, filepath.Join(".pi", "agent"), filepath.Join("pi", "agent")),
		OnEvent: func(e sandboxrt.Event) {
			// Names and ids only; never environment values or secrets.
			slog.Info(e.Kind, "run", e.RunID, "thread", e.ThreadID, "backend", e.Backend, "detail", e.Detail)
		},
	})
	if err != nil {
		slog.Error("sandbox misconfigured; all agent runs will be rejected", "mode", mode, "error", err)
		return sandboxrt.Failed(err)
	}
	slog.Info("sandbox enabled", "mode", mode, "backend", cfg.Agent.SandboxBackend)
	if clisetup.ResolveCLI(cfg.Agent.CLI) == "claude" && !clisetup.SandboxedClaudeAuth(cfg.Agent.SandboxEnvAllow, cfg.Agent.SandboxModelProxy) {
		slog.Warn("sandbox: claude runs have no credentials: " + clisetup.SandboxedClaudeAuthHelp)
	}
	return rt
}

// sandboxPrivatePaths lists the runner's own files a sandboxed agent must not
// read: its .env files (bot tokens, API keys, git tokens) and its state, logs,
// outputs, workspaces and repo cache. The memory dir and uploads stay
// readable: prompts point the agent at memory, and uploaded files reach it by
// path.
func sandboxPrivatePaths(cfg *config.Config) []string {
	paths := []string{cfg.StateRoot, cfg.TmpRoot, cfg.LogsRoot, cfg.OutputsRoot, cfg.RepoCacheRoot}
	if d := codexLoginDir(cfg); d != "" {
		// The whole directory: a refresh writes the rotated token to a temp
		// file next to auth.json first.
		paths = append(paths, d)
	}
	for _, dir := range []string{cfg.ProjectDir, cfg.DataDir} {
		if dir == "" {
			continue
		}
		// Listed even before they exist (/set creates .env.local later): each
		// run checks the list again.
		paths = append(paths, filepath.Join(dir, ".env"), filepath.Join(dir, ".env.local"))
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if n := e.Name(); strings.HasPrefix(n, ".env.") && n != ".env.local" {
				paths = append(paths, filepath.Join(dir, n))
			}
		}
	}
	return paths
}

// codexLogin is the codex ChatGPT login sandboxed codex uses, and whether it
// was set explicitly. AGENT_SANDBOX_CODEX_LOGIN, if set ("off" for none);
// else, with AGENT_CLI=codex and no OPENAI_API_KEY, the login codex itself
// uses here ($CODEX_HOME, ~/.codex, or agent-home/codex when isolated), if
// that is a ChatGPT login.
func codexLogin(cfg *config.Config) (path string, explicit bool) {
	switch v := cfg.Agent.SandboxCodexLogin; v {
	case "off":
		return "", true
	case "":
	default:
		return v, true
	}
	if cfg.Agent.CLI != "codex" || os.Getenv("OPENAI_API_KEY") != "" {
		return "", false
	}
	dir := os.Getenv("CODEX_HOME")
	if cfg.Agent.Isolated {
		dir = filepath.Join(cfg.ProjectDir, agenthome.Dir, "codex")
	} else if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(home, ".codex")
	}
	path = filepath.Join(dir, "auth.json")
	if (&codexlogin.Login{Path: path}).Check() != nil {
		return "", false // none, or an API-key login: nothing to default to
	}
	return path, false
}

// codexLoginDir is the directory of the codex login, which the sandbox must
// not read: "" when there is none, or when it is / or the home directory
// (hiding those would hide everything).
func codexLoginDir(cfg *config.Config) string {
	path, _ := codexLogin(cfg)
	if path == "" {
		return ""
	}
	d, err := filepath.Abs(filepath.Dir(path))
	if err != nil || d == "/" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == d {
		return ""
	}
	return d
}

// sandboxClaudeSeed is where a sandboxed claude CLI's settings, skills and MCP
// servers come from: agent-home/claude when the runner is isolated, else the
// host user's ~/.claude and ~/.claude.json. Credentials are never copied.
func sandboxClaudeSeed(cfg *config.Config) sandboxrt.ClaudeSeed {
	if cfg.Agent.Isolated {
		dir := filepath.Join(cfg.ProjectDir, agenthome.Dir, "claude")
		return sandboxrt.ClaudeSeed{Dir: dir, StateFile: filepath.Join(dir, ".claude.json")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return sandboxrt.ClaudeSeed{}
	}
	return sandboxrt.ClaudeSeed{Dir: filepath.Join(home, ".claude"), StateFile: filepath.Join(home, ".claude.json")}
}

// llmConfiner confines the fast-LLM clients' executor fallback (analyzer,
// curator, planner outside a session) when the sandbox is on: each call gets
// a sandbox run with a throwaway workspace. The calls share one sandbox
// thread, so they also share one sandbox home (claude's config dir) and run
// one at a time. Nil when the sandbox is off.
func (h *Handlers) llmConfiner() llm.Confiner {
	rt := h.sandbox
	if rt == nil || rt.Mode() == sandbox.ModeOff {
		return nil
	}
	root := h.config.TmpRoot
	return func(ctx context.Context) (context.Context, func(), error) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, nil, err
		}
		dir, err := os.MkdirTemp(root, "llm-")
		if err != nil {
			return nil, nil, err
		}
		run, err := rt.Begin(ctx, sandboxrt.BeginReq{ThreadID: "llm-fallback", RunID: filepath.Base(dir), Workspace: dir, MaxSecond: 300})
		if err != nil {
			os.RemoveAll(dir)
			return nil, nil, err
		}
		return run.Ctx, func() {
			run.Finish()
			os.RemoveAll(dir)
		}, nil
	}
}

// sandboxModelChannels are the credentials the per-run model proxies attach
// for sandboxed CLIs, by provider:
//   - Anthropic (claude, pi): a `claude setup-token` token, sent as a bearer
//     token (the API accepts it for subscription use), or else an API key;
//     to ANTHROPIC_BASE_URL or the Anthropic API.
//   - OpenAI (codex, pi): OPENAI_API_KEY; to OPENAI_BASE_URL or the OpenAI API.
//     Or, with AGENT_SANDBOX_CODEX_LOGIN, a codex ChatGPT login (~/.codex's or the runner's
//     own auth.json, refreshed here) to ChatGPT's codex backend: codex only.
//
// Nil when AGENT_SANDBOX_MODEL_PROXY is off.
func sandboxModelChannels(cfg *config.Config) map[string]*sandboxrt.ModelChannel {
	if !cfg.Agent.SandboxModelProxy {
		return nil
	}
	out := map[string]*sandboxrt.ModelChannel{}
	if u := upstreamURL("ANTHROPIC_BASE_URL", "https://api.anthropic.com"); u != nil {
		switch {
		case os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "":
			out[sandboxrt.ProviderAnthropic] = &sandboxrt.ModelChannel{Upstream: u, Paths: sandboxrt.AnthropicPaths, OAuth: true,
				AuthHeader: "Authorization", AuthValue: "Bearer " + os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")}
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			out[sandboxrt.ProviderAnthropic] = &sandboxrt.ModelChannel{Upstream: u, Paths: sandboxrt.AnthropicPaths,
				AuthHeader: "x-api-key", AuthValue: os.Getenv("ANTHROPIC_API_KEY")}
		}
	}
	if path, explicit := codexLogin(cfg); path != "" {
		login := &codexlogin.Login{Path: path}
		if codexLoginDir(cfg) == "" {
			slog.Warn("sandbox: AGENT_SANDBOX_CODEX_LOGIN must be in a directory of its own (it is hidden from the sandbox), not / or the home directory; codex has no ChatGPT channel", "path", path)
		} else if err := login.Check(); err != nil {
			slog.Warn("sandbox: AGENT_SANDBOX_CODEX_LOGIN unusable; codex has no ChatGPT channel", "error", err)
		} else {
			if !explicit {
				slog.Info("sandbox: codex uses its ChatGPT login through the model proxy (AGENT_SANDBOX_CODEX_LOGIN=off to stop)", "path", path)
			}
			u, _ := url.Parse("https://chatgpt.com")
			out[sandboxrt.ProviderOpenAI] = &sandboxrt.ModelChannel{Upstream: u, Paths: sandboxrt.ChatGPTPaths,
				Credential: login.Headers, ChatGPT: true}
			return out
		}
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		if u := upstreamURL("OPENAI_BASE_URL", "https://api.openai.com"); u != nil {
			out[sandboxrt.ProviderOpenAI] = &sandboxrt.ModelChannel{Upstream: u, Paths: sandboxrt.OpenAIPaths,
				AuthHeader: "Authorization", AuthValue: "Bearer " + key}
		}
	}
	return out
}

// upstreamURL is env's base URL, or def. The proxy's paths start at /v1, so a
// trailing /v1 (OPENAI_BASE_URL usually has one) is dropped.
func upstreamURL(env, def string) *url.URL {
	v := os.Getenv(env)
	if v == "" {
		v = def
	}
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSuffix(v, "/"), "/v1"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		slog.Warn("sandbox: model proxy: bad "+env+"; that provider is not proxied", "value", v)
		return nil
	}
	return u
}

// sandboxSeedDir is where a sandboxed CLI's allowlisted config is copied
// from: agent-home/<isolated> when the runner is isolated, else ~/<host>.
func sandboxSeedDir(cfg *config.Config, host, isolated string) string {
	if cfg.Agent.Isolated {
		return filepath.Join(cfg.ProjectDir, agenthome.Dir, isolated)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, host)
}
