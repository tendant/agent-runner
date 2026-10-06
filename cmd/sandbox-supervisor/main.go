// Command sandbox-supervisor enforces sandbox deadlines and reaps orphaned
// sandboxes independently of the runner, so limits survive a runner crash.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox/lease"
	"github.com/agent-runner/agent-runner/internal/sandbox/supervisor"
)

func main() {
	dir := flag.String("leases", "", "lease directory (required)")
	every := flag.Duration("interval", 2*time.Second, "sweep interval")
	grace := flag.Duration("grace", 5*time.Second, "TERM to KILL grace")
	once := flag.Bool("once", false, "sweep once and exit")
	flag.Parse()
	if *dir == "" {
		slog.Error("--leases is required")
		os.Exit(2)
	}
	store, err := lease.NewStore(*dir)
	if err != nil {
		slog.Error("open lease dir", "err", err)
		os.Exit(1)
	}
	s := &supervisor.Supervisor{Store: store, Grace: *grace, OnEvent: func(kind string, r lease.Record) {
		// Names and ids only: never environment values or secrets.
		slog.Warn(kind, "lease", r.Name, "run", r.RunID, "thread", r.ThreadID, "owner_pid", r.Owner.PID)
	}}
	if *once {
		n, err := s.Sweep()
		slog.Info("sweep", "acted", n, "err", err)
		return
	}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	s.Run(*every, stop)
}
