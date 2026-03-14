package api

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	// Import metrics package to register prometheus collectors.
	_ "github.com/agent-runner/agent-runner/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/agent-runner/agent-runner/internal/conversation"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
	"github.com/agent-runner/agent-runner/internal/runner"
	"github.com/agent-runner/agent-runner/internal/stream"
	"github.com/agent-runner/agent-runner/internal/telegram"
)

// Server represents the HTTP API server
type Server struct {
	config       *config.Config
	httpServer   *http.Server
	handlers     *Handlers
	telegramBot  *telegram.Bot
	streamBot    *stream.Bot
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
	exec := executor.NewExecutor(cfg.Agent.CLI, cfg.Agent.Model, cfg.Agent.MaxTurns)
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
	mux.HandleFunc("/agent", handlers.HandleStartAgent)
	mux.HandleFunc("/agent/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			handlers.HandleStopAgent(w, r)
		} else {
			handlers.HandleGetAgent(w, r)
		}
	})

	// Apply middleware
	var handler http.Handler = mux
	handler = loggingMiddleware(handler)
	if cfg.API.APIKey != "" {
		handler = apiKeyMiddleware(cfg.API.APIKey, handler)
	}

	// Create conversation components for Telegram bot
	convManager := conversation.NewManager()
	analyzer := conversation.NewAnalyzer(exec)
	agentStarter := NewAgentStarterAdapter(handlers)
	telegramBot := telegram.New(cfg.Telegram, agentStarter, convManager, analyzer)
	streamBot := stream.New(cfg.Stream, agentStarter, convManager, analyzer)
	if streamBot != nil {
		handlers.SetNotifier(streamBot)
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

	// Startup cleanup
	if s.config.StartupCleanupStaleJobs {
		workspaceManager := executor.NewWorkspaceManager(s.config.TmpRoot, s.config.MaxRuntimeSeconds)
		if err := workspaceManager.CleanupStaleWorkspaces(); err != nil {
			slog.Warn("failed to cleanup stale workspaces", "error", err)
		}
	}

	// Setup graceful shutdown
	done := make(chan bool, 1)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		slog.Info("server shutting down")

		// Stop runner first (it may be executing a task)
		if s.runner != nil {
			s.runner.Stop()
		}

		// Stop bots
		if s.streamBot != nil {
			s.streamBot.Stop()
		}
		if s.telegramBot != nil {
			s.telegramBot.Stop()
		}
		s.convManager.Stop()
		s.agentManager.Stop()
		s.jobManager.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		s.httpServer.SetKeepAlivesEnabled(false)
		if err := s.httpServer.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v", err)
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

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}

	<-done
	slog.Info("server stopped")
	return nil
}

func (s *Server) ensureDirectories() error {
	dirs := []string{
		s.config.ReposRoot,
		s.config.LogsRoot,
		s.config.TmpRoot,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
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

