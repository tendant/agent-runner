package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	// Import metrics package to register prometheus collectors.
	_ "github.com/agent-runner/agent-runner/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/agent-runner/agent-runner/internal/conversation"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/llm"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/runner"
	"github.com/agent-runner/agent-runner/internal/stream"
	"github.com/agent-runner/agent-runner/internal/telegram"
	"github.com/agent-runner/agent-runner/internal/wechat"
)

// Server represents the HTTP API server
type Server struct {
	config       *config.Config
	httpServer   *http.Server
	handlers     *Handlers
	telegramBot  *telegram.Bot
	streamBot    *stream.Bot
	wechatBot    *wechat.Bot
	jobManager   *jobs.Manager
	agentManager *agent.Manager
	convManager  *conversation.Manager
	runner       *runner.HybridRunner
}

// NewServer creates a new API server
func NewServer(cfg *config.Config) *Server {
	// Initialize components
	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentManager := agent.NewManager(cfg.JobRetentionSeconds, cfg.Agent.MaxQueueSize)
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	gitOps.Token = cfg.GitToken
	// Level 3: agent CLI executor — uses reasoning model/provider (or CLI default if unset).
	exec := executor.NewExecutor(cfg.Agent.CLI, cfg.Agent.ReasoningProvider, cfg.Agent.ReasoningModel, cfg.Agent.MaxTurns)
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.LogsRoot)

	handlers := NewHandlers(cfg, jobManager, agentManager, gitOps, exec, validator, workspaceManager, runLogger)

	// Setup router
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handlers.HandleHealth)
	mux.HandleFunc("/notify", handlers.HandleNotify)
	mux.HandleFunc("/run", handlers.HandleRun)
	mux.HandleFunc("/job/", handlers.HandleGetJob)
	mux.HandleFunc("/status/", handlers.HandleGetStatus)
	mux.HandleFunc("/projects", handlers.HandleGetProjects)
	mux.HandleFunc("/schedule", handlers.HandleSchedule)
	mux.HandleFunc("/schedule/", handlers.HandleDeleteSchedule)
	mux.HandleFunc("/schedules", handlers.HandleListSchedules)
	mux.HandleFunc("/debug/schedules", handlers.HandleDebugSchedules)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/bootstrap", handlers.HandleBootstrap)
	mux.HandleFunc("/agent", handlers.HandleStartAgent)
	mux.HandleFunc("/agent/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stop"):
			handlers.HandleStopAgent(w, r)
		case strings.HasSuffix(r.URL.Path, "/stream"):
			handlers.HandleStreamAgent(w, r)
		default:
			handlers.HandleGetAgent(w, r)
		}
	})
	mux.HandleFunc("/sessions", handlers.HandleListSessions)

	// Apply middleware
	var handler http.Handler = mux
	handler = loggingMiddleware(handler)
	if cfg.API.APIKey != "" {
		handler = apiKeyMiddleware(cfg.API.APIKey, handler)
	}

	// Level 2: direct LLM client for fast planning/response — uses AGENT_MODEL/AGENT_PROVIDER.
	// Falls back to ExecutorClient when no API credentials are configured.
	plannerClient := llm.NewClient(llm.Config{
		Provider:  cfg.Agent.Provider,
		Model:     cfg.Agent.Model,
		MaxTokens: 4096,
	}, exec)
	handlers.SetPlannerClient(plannerClient)

	// Create conversation components for Telegram bot
	convManager := conversation.NewManager(filepath.Join(cfg.TmpRoot, "conversations"))
	llmClient := llm.NewClient(llm.Config{
		Provider: cfg.Analyzer.Provider,
		Model:    cfg.Analyzer.Model,
		APIKey:   cfg.Analyzer.APIKey,
		BaseURL:  cfg.Analyzer.BaseURL,
	}, exec)
	analyzer := conversation.NewAnalyzer(llmClient)
	if cfg.Analyzer.TimeoutSeconds > 0 {
		analyzer.SetTimeout(time.Duration(cfg.Analyzer.TimeoutSeconds) * time.Second)
	}
	if cfg.Agent.PromptFile != "" {
		if data, err := os.ReadFile(cfg.Agent.PromptFile); err == nil {
			analyzer.SetAgentContext(string(data))
		} else {
			slog.Warn("analyzer: could not read prompt file for context", "path", cfg.Agent.PromptFile, "error", err)
		}
	} else if cfg.Agent.SystemPrompt != "" {
		if data, err := os.ReadFile(cfg.Agent.SystemPrompt); err == nil {
			analyzer.SetAgentContext(string(data))
		} else {
			slog.Warn("analyzer: could not read system prompt for context", "path", cfg.Agent.SystemPrompt, "error", err)
		}
	}
	commander := NewCommander(cfg, handlers)
	handlers.SetCommander(commander) // also builds handlers.gateway
	gateway := handlers.Gateway()
	agentStarter := NewAgentStarterAdapter(handlers)
	telegramBot := telegram.New(cfg.Telegram, agentStarter, convManager, analyzer, cfg.TmpRoot, gateway)
	streamBot := stream.New(cfg.Stream, cfg.UploadsRoot, agentStarter, convManager, analyzer, gateway)
	wechatBot := wechat.New(cfg.WeChat, agentStarter, convManager, analyzer, gateway)

	// Wire MultiNotifier: fan out background notifications to all active bots.
	// Chat-initiated sessions (stream/telegram/wechat) skip notifySessionResult
	// entirely; this path is only reached for API/runner-initiated sessions.
	var notifiers []Notifier
	if streamBot != nil {
		notifiers = append(notifiers, streamBot)
	}
	if telegramBot != nil {
		notifiers = append(notifiers, telegramBot)
	}
	if len(notifiers) > 0 {
		handlers.SetNotifier(NewMultiNotifier(notifiers...))
	}
	if streamBot != nil {
		streamBot.SetWeChatReloader(wechatBot.Reload, cfg.WeChat.BaseURL)
	}

	return &Server{
		config: cfg,
		httpServer: &http.Server{
			Addr:         cfg.API.Bind,
			Handler:      handler,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		handlers:     handlers,
		telegramBot:  telegramBot,
		streamBot:    streamBot,
		wechatBot:    wechatBot,
		jobManager:   jobManager,
		agentManager: agentManager,
		convManager:  convManager,
	}
}

// Start starts the HTTP server with graceful shutdown support
func (s *Server) Start() error {
	// Ensure directories exist
	if err := s.ensureDirectories(); err != nil {
		return err
	}

	// Warn if the configured agent CLI is not installed.
	cli := s.config.Agent.CLI
	if cli == "" {
		cli = "opencode"
	}
	if !CLIInstalled(cli) {
		slog.Warn("agent CLI not found in PATH — run /install-cli or /bootstrap to install", "cli", cli, "install", cliInstallHint(cli))
	}

	// Startup cleanup
	if s.config.StartupCleanupStaleJobs {
		workspaceManager := executor.NewWorkspaceManager(s.config.TmpRoot, s.config.MaxRuntimeSeconds)
		if err := workspaceManager.CleanupStaleWorkspaces(); err != nil {
			slog.Warn("failed to cleanup stale workspaces", "error", err)
		}
	}
	if s.config.LogRetentionDays > 0 {
		if err := logging.CleanupOldLogs(s.config.LogsRoot, s.config.LogRetentionDays); err != nil {
			slog.Warn("failed to cleanup old logs", "error", err)
		} else {
			slog.Info("log retention applied", "max_days", s.config.LogRetentionDays)
		}
	}

	// Setup graceful shutdown
	done := make(chan struct{})
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		// Restore default signal behaviour immediately so a second Ctrl-C kills
		// the process via the OS default handler, bypassing Go's scheduler
		// entirely. This works even if a goroutine is stuck in a blocking call.
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		slog.Info("server shutting down — Ctrl-C again to force quit")

		// Force-exit after 5 seconds regardless — don't wait for stuck goroutines.
		go func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				slog.Warn("shutdown timed out, forcing exit")
				os.Exit(1)
			}
		}()

		// Cancel agent/job contexts first so running sessions stop promptly.
		s.agentManager.Stop()
		s.jobManager.Stop()

		// Stop runner (it may be executing a task).
		if s.runner != nil {
			s.runner.Stop()
		}

		// Stop bots in parallel with a 3-second cap — a hung SSE/poll loop
		// must not block the whole shutdown past the 5-second force-exit above.
		var botWg sync.WaitGroup
		for _, fn := range []func(){
			func() {
				if s.wechatBot != nil {
					s.wechatBot.Stop()
				}
			},
			func() {
				if s.streamBot != nil {
					s.streamBot.Stop()
				}
			},
			func() {
				if s.telegramBot != nil {
					s.telegramBot.Stop()
				}
			},
			s.convManager.Stop,
		} {
			botWg.Add(1)
			go func(f func()) {
				defer botWg.Done()
				f()
			}(fn)
		}
		botDone := make(chan struct{})
		go func() { botWg.Wait(); close(botDone) }()
		select {
		case <-botDone:
		case <-time.After(3 * time.Second):
			slog.Warn("bot shutdown timed out, proceeding")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		s.httpServer.SetKeepAlivesEnabled(false)
		if err := s.httpServer.Shutdown(ctx); err != nil {
			slog.Warn("http server shutdown error", "error", err)
		}
		close(done)
	}()

	// Bind the port first so we know the server is ready before starting
	// bots and other components that depend on the API being reachable.
	ln, err := net.Listen("tcp", s.config.API.Bind)
	if err != nil {
		return err
	}
	slog.Info("server listening", "addr", ln.Addr().String())

	// Start Telegram bot
	if s.telegramBot != nil {
		if err := s.telegramBot.Start(context.Background()); err != nil {
			slog.Warn("telegram bot start failed", "error", err)
		}
	}

	// Start stream bot
	if s.streamBot != nil {
		if err := s.streamBot.Start(context.Background()); err != nil {
			slog.Warn("stream bot start failed", "error", err)
		}
	}

	// Start WeChat bot
	if s.wechatBot != nil {
		if err := s.wechatBot.Start(context.Background()); err != nil {
			slog.Warn("wechat bot start failed", "error", err)
		}
	}

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}

	<-done
	slog.Info("server stopped")
	return nil
}

func (s *Server) ensureDirectories() error {
	dirs := []string{
		s.config.RepoCacheRoot,
		s.config.LogsRoot,
		s.config.TmpRoot,
		filepath.Join(s.config.TmpRoot, "conversations"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	// Write a .gitignore to runtime dirs so their contents are never tracked by a parent git repo.
	for _, dir := range []string{s.config.TmpRoot, s.config.LogsRoot, s.config.RepoCacheRoot} {
		gi := filepath.Join(dir, ".gitignore")
		if _, err := os.Stat(gi); os.IsNotExist(err) {
			_ = os.WriteFile(gi, []byte("*\n"), 0644)
		}
	}

	return nil
}

// loggingMiddleware logs all requests
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration", time.Since(start),
		)
	})
}

// apiKeyMiddleware validates the X-API-Key header
func apiKeyMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		providedKey := r.Header.Get("X-API-Key")
		if providedKey != apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Invalid or missing API key"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController traverse the wrapper chain so that
// SetWriteDeadline / SetReadDeadline reach the underlying net/http connection.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Handler returns the HTTP handler for use in tests
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Handlers returns the server's Handlers instance for runner bridge setup.
func (s *Server) Handlers() *Handlers {
	return s.handlers
}

// SetRunner sets the hybrid runner on the server for lifecycle management.
func (s *Server) SetRunner(r *runner.HybridRunner) {
	s.runner = r
}

