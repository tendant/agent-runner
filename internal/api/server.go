package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/agent"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/conversation"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
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
}

// NewServer creates a new API server
func NewServer(cfg *config.Config) *Server {
	// Initialize components
	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	agentManager := agent.NewManager(cfg.JobRetentionSeconds, cfg.Agent.MaxQueueSize)
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor(cfg.Agent.Model, cfg.Agent.MaxTurns)
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
			log.Printf("Warning: failed to cleanup stale workspaces: %v", err)
		}
	}

	// Setup graceful shutdown
	done := make(chan bool, 1)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Server is shutting down...")

		// Stop bots first
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

	// Start Telegram bot
	if s.telegramBot != nil {
		if err := s.telegramBot.Start(context.Background()); err != nil {
			log.Printf("Warning: %v", err)
		}
	}

	// Start stream bot
	if s.streamBot != nil {
		if err := s.streamBot.Start(context.Background()); err != nil {
			log.Printf("Warning: stream bot: %v", err)
		}
	}

	log.Printf("Server starting on %s", s.config.API.Bind)

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	<-done
	log.Println("Server stopped")
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

		log.Printf("%s %s %d %v",
			r.Method,
			r.URL.Path,
			wrapped.statusCode,
			time.Since(start),
		)
	})
}

// apiKeyMiddleware validates the X-API-Key header
func apiKeyMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
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

