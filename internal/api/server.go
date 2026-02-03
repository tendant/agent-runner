package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/jobs"
	"github.com/agent-runner/agent-runner/internal/logging"
)

// Server represents the HTTP API server
type Server struct {
	config     *config.Config
	httpServer *http.Server
	handlers   *Handlers
}

// NewServer creates a new API server
func NewServer(cfg *config.Config) *Server {
	// Initialize components
	jobManager := jobs.NewManager(cfg.JobRetentionSeconds, cfg.MaxConcurrentJobs)
	gitOps := git.NewOperations(cfg.GitPushRetries, cfg.GitPushRetryDelaySeconds)
	exec := executor.NewExecutor()
	validator := executor.NewValidator(cfg.Validation.BlockedPaths, cfg.Validation.BlockBinaryFiles)
	workspaceManager := executor.NewWorkspaceManager(cfg.TmpRoot, cfg.MaxRuntimeSeconds)
	runLogger := logging.NewRunLogger(cfg.RunsRoot)

	handlers := NewHandlers(cfg, jobManager, gitOps, exec, validator, workspaceManager, runLogger)

	// Setup router
	mux := http.NewServeMux()
	mux.HandleFunc("/run", handlers.HandleRun)
	mux.HandleFunc("/job/", handlers.HandleGetJob)
	mux.HandleFunc("/status/", handlers.HandleGetStatus)
	mux.HandleFunc("/projects", handlers.HandleGetProjects)

	// Apply middleware
	var handler http.Handler = mux
	handler = loggingMiddleware(handler)
	if cfg.API.APIKey != "" {
		handler = apiKeyMiddleware(cfg.API.APIKey, handler)
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
		handlers: handlers,
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

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		s.httpServer.SetKeepAlivesEnabled(false)
		if err := s.httpServer.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v", err)
		}
		close(done)
	}()

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
		s.config.ProjectsRoot,
		s.config.RunsRoot,
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
