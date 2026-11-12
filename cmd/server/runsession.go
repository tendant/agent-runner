package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-runner/agent-runner/internal/api"
	"github.com/agent-runner/agent-runner/internal/config"
	"github.com/agent-runner/agent-runner/internal/execution"
	"github.com/agent-runner/agent-runner/internal/tracing"
)

// runSession is `agent-runner run-session -session <id> -dispatcher <url>`
// (JOBS_DESIGN.md §5, §7): it runs one session for the dispatcher that
// started it and exits when the result is delivered. The token comes in
// AGENT_JOB_TOKEN, out of the process list. The configuration is the
// dispatcher's: the same environment and DATA_DIR, or (-config -) what the
// dispatcher writes to stdin. Exit status 0: the
// result was delivered; 1: it was not, or the session could not be fetched;
// 2: bad usage.
func runSession(args []string) int {
	fs := flag.NewFlagSet("run-session", flag.ContinueOnError)
	sessionID := fs.String("session", "", "the session to run")
	dispatcher := fs.String("dispatcher", os.Getenv("AGENT_JOB_DISPATCHER_URL"), "the dispatcher's base URL (AGENT_JOB_DISPATCHER_URL)")
	configFrom := fs.String("config", "", `"-": read the dispatcher's configuration as JSON on stdin (the subprocess backend) instead of loading it from the environment`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := os.Getenv("AGENT_JOB_TOKEN")
	if *sessionID == "" || *dispatcher == "" || token == "" {
		fmt.Fprintln(os.Stderr, "run-session: -session, -dispatcher (or AGENT_JOB_DISPATCHER_URL) and AGENT_JOB_TOKEN are required")
		return 2
	}

	var cfg *config.Config
	var err error
	switch *configFrom {
	case "":
		cfg, err = config.LoadFromEnv()
	case "-":
		cfg, err = execution.ReadRunnerConfig(os.Stdin)
	default:
		fmt.Fprintln(os.Stderr, `run-session: -config takes only "-"`)
		return 2
	}
	if err != nil {
		slog.Error("run-session: config", "error", err)
		return 1
	}
	shutdownTracing, err := tracing.Init(context.Background(), tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		ServiceName: cfg.Tracing.ServiceName,
		Protocol:    cfg.Tracing.Protocol,
		Version:     buildTime,
	})
	if err != nil {
		slog.Error("run-session: tracing", "error", err)
		return 1
	}
	defer shutdownTracing(context.Background())

	// SIGTERM (a Job deleted, the dispatcher's Stop) ends the run the way a
	// shutdown does; the result is still sent.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	slog.Info("run-session starting", "session_id", *sessionID, "dispatcher", *dispatcher, "built", buildTime)
	eng := api.NewRunner(cfg, *sessionID)
	err = eng.RunRemote(ctx, &execution.JobClient{
		URL:       *dispatcher,
		SessionID: *sessionID,
		Token:     token,
		RetryFor:  cfg.Agent.JobReportRetry,
	})
	if err != nil {
		slog.Error("run-session failed", "session_id", *sessionID, "error", err)
		return 1
	}
	slog.Info("run-session done", "session_id", *sessionID)
	return 0
}
