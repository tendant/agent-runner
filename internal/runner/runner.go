package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	simpleworkflow "github.com/tendant/simple-workflow"
)

// AgentTaskPayload is the JSON payload stored in workflow_run.payload.
type AgentTaskPayload struct {
	Message         string   `json:"message"`
	Paths           []string `json:"paths,omitempty"`
	Author          string   `json:"author,omitempty"`
	MaxIterations   int      `json:"max_iterations,omitempty"`
	MaxTotalSeconds int      `json:"max_total_seconds,omitempty"`
}

// AgentExecutor is the interface the runner uses to invoke agent execution.
type AgentExecutor interface {
	ExecuteAgentTask(ctx context.Context, payload AgentTaskPayload) error
}

// HybridRunner pulls agent tasks from workflow_run via simple-workflow,
// processes them through AgentExecutor, and tracks status via heartbeat.
type HybridRunner struct {
	db         *sql.DB
	dialect    simpleworkflow.Dialect
	driverName string
	runs       *simpleworkflow.RunRepository
	heartbeat  *HeartbeatStore
	listener   *Listener
	executor   AgentExecutor

	agentID       string
	leaseDuration time.Duration
	pollCap       time.Duration
	hbInterval    time.Duration
	typePrefix    string
	connString    string
}

// Config holds runner configuration.
type Config struct {
	DatabaseURL       string
	AgentID           string
	LeaseDuration     time.Duration
	PollCap           time.Duration
	HeartbeatInterval time.Duration
	MaxAttempts       int
	TypePrefix        string
}

// New creates a new HybridRunner.
func New(cfg Config, executor AgentExecutor) (*HybridRunner, error) {
	dialect, dsn, err := simpleworkflow.DetectDialect(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("detect dialect: %w", err)
	}

	db, err := dialect.OpenDB(dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	driverName := dialect.DriverName()

	agentID := cfg.AgentID
	if agentID == "" {
		hostname, _ := os.Hostname()
		agentID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	return &HybridRunner{
		db:            db,
		dialect:       dialect,
		driverName:    driverName,
		runs:          simpleworkflow.NewRunRepository(db, dialect),
		heartbeat:     NewHeartbeatStore(db, driverName),
		executor:      executor,
		agentID:       agentID,
		leaseDuration: cfg.LeaseDuration,
		pollCap:       cfg.PollCap,
		hbInterval:    cfg.HeartbeatInterval,
		typePrefix:    cfg.TypePrefix,
		connString:    cfg.DatabaseURL,
	}, nil
}

// Start runs migrations, starts the listener (postgres only), and enters the main loop.
// Blocks until ctx is cancelled.
func (r *HybridRunner) Start(ctx context.Context) error {
	// Run simple-workflow migrations
	swClient := simpleworkflow.NewClientWithDB(r.db, r.dialect)
	if err := swClient.AutoMigrate(ctx); err != nil {
		return fmt.Errorf("simple-workflow migrate: %w", err)
	}

	// Run runner-specific migrations (agent_heartbeat + trigger)
	if err := AutoMigrate(ctx, r.db, r.driverName); err != nil {
		return fmt.Errorf("runner migrate: %w", err)
	}

	// Start LISTEN/NOTIFY listener (postgres only, nil on sqlite)
	r.listener = NewListener(r.driverName, r.connString)

	// Mark idle
	r.heartbeat.Upsert(ctx, r.agentID, "idle", "", "", "")

	log.Printf("runner: started (agent_id=%s driver=%s lease=%s poll_cap=%s heartbeat=%s prefix=%s)",
		r.agentID, r.driverName, r.leaseDuration, r.pollCap, r.hbInterval, r.typePrefix)

	r.loop(ctx)
	return nil
}

// Stop cleans up resources.
func (r *HybridRunner) Stop() {
	if r.listener != nil {
		r.listener.Close()
	}
	r.db.Close()
}

func (r *HybridRunner) loop(ctx context.Context) {
	pollTimer := time.NewTimer(r.pollCap)
	defer pollTimer.Stop()

	heartbeatTimer := time.NewTicker(r.hbInterval)
	defer heartbeatTimer.Stop()

	wakeCh := r.listener.Wake() // never-fires channel if listener is nil

	for {
		// Try to claim and execute one task
		claimed := r.claimAndExecuteOne(ctx)
		if claimed {
			// Reset poll timer — tight loop when work available
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollTimer.Reset(r.pollCap)
			continue
		}

		// No work available — wait for a signal
		select {
		case <-wakeCh:
			// NOTIFY received — try claim immediately
		case <-pollTimer.C:
			pollTimer.Reset(r.pollCap)
		case <-heartbeatTimer.C:
			r.heartbeat.Touch(ctx, r.agentID)
		case <-ctx.Done():
			log.Printf("runner: shutting down")
			r.heartbeat.Upsert(ctx, r.agentID, "stopped", "", "", "")
			return
		}
	}
}

func (r *HybridRunner) claimAndExecuteOne(ctx context.Context) bool {
	// Ensure type prefix ends with % for LIKE matching
	prefix := r.typePrefix
	if len(prefix) > 0 && prefix[len(prefix)-1] != '%' {
		prefix += "%"
	}
	typePrefixes := []string{prefix}

	run, err := r.runs.Claim(ctx, r.agentID, typePrefixes, r.leaseDuration)
	if err != nil {
		log.Printf("runner: claim error: %v", err)
		return false
	}
	if run == nil {
		return false
	}

	log.Printf("runner: claimed task %s (type=%s attempt=%d)", run.ID, run.Type, run.Attempt)

	// Parse payload
	var payload AgentTaskPayload
	if err := json.Unmarshal(run.Payload, &payload); err != nil {
		log.Printf("runner: invalid payload for %s: %v", run.ID, err)
		r.runs.MarkFailed(ctx, run, fmt.Errorf("invalid payload: %w", err))
		return true
	}

	// Update heartbeat to running
	r.heartbeat.Upsert(ctx, r.agentID, "running", run.ID, "", payload.Message)

	// Start lease extension goroutine
	leaseCtx, leaseCancel := context.WithCancel(ctx)
	defer leaseCancel()
	go r.extendLease(leaseCtx, run.ID)

	// Execute the agent task
	execErr := r.executor.ExecuteAgentTask(ctx, payload)

	// Mark result
	if execErr != nil {
		log.Printf("runner: task %s failed: %v", run.ID, execErr)
		r.runs.MarkFailed(ctx, run, execErr)
		r.heartbeat.Upsert(ctx, r.agentID, "idle", "", run.ID, "")
	} else {
		log.Printf("runner: task %s succeeded", run.ID)
		r.runs.MarkSucceeded(ctx, run.ID, map[string]string{"status": "completed"})
		r.heartbeat.Upsert(ctx, r.agentID, "idle", "", run.ID, "")
	}

	return true
}

// extendLease extends the lease at 50% of lease duration until ctx is cancelled.
func (r *HybridRunner) extendLease(ctx context.Context, runID string) {
	interval := r.leaseDuration / 2
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.runs.ExtendLease(ctx, runID, r.leaseDuration); err != nil {
				log.Printf("runner: lease extension failed for %s: %v", runID, err)
				return
			}
		}
	}
}
