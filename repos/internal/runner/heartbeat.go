package runner

import (
	"context"
	"database/sql"
	"time"
)

// HeartbeatStore manages the agent_heartbeat table.
type HeartbeatStore struct {
	db         *sql.DB
	driverName string
}

// NewHeartbeatStore creates a new HeartbeatStore.
func NewHeartbeatStore(db *sql.DB, driverName string) *HeartbeatStore {
	return &HeartbeatStore{db: db, driverName: driverName}
}

// Upsert inserts or updates a heartbeat row.
func (s *HeartbeatStore) Upsert(ctx context.Context, agentID, status, currentTask, lastCompleted, nextStep string) error {
	var query string
	switch s.driverName {
	case "postgres":
		query = `
			INSERT INTO agent_heartbeat (agent_id, status, current_task, last_completed, next_step, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (agent_id) DO UPDATE SET
				status = $2, current_task = $3, last_completed = $4, next_step = $5, updated_at = NOW()`
	default:
		query = `
			INSERT INTO agent_heartbeat (agent_id, status, current_task, last_completed, next_step, updated_at)
			VALUES (?, ?, ?, ?, ?, datetime('now'))
			ON CONFLICT (agent_id) DO UPDATE SET
				status = excluded.status, current_task = excluded.current_task,
				last_completed = excluded.last_completed, next_step = excluded.next_step,
				updated_at = datetime('now')`
	}

	_, err := s.db.ExecContext(ctx, query, agentID, status, nullStr(currentTask), nullStr(lastCompleted), nullStr(nextStep))
	return err
}

// Touch updates the updated_at timestamp for the given agent.
func (s *HeartbeatStore) Touch(ctx context.Context, agentID string) error {
	var query string
	switch s.driverName {
	case "postgres":
		query = `UPDATE agent_heartbeat SET updated_at = NOW() WHERE agent_id = $1`
	default:
		query = `UPDATE agent_heartbeat SET updated_at = datetime('now') WHERE agent_id = ?`
	}
	_, err := s.db.ExecContext(ctx, query, agentID)
	return err
}

// Heartbeat represents a row in agent_heartbeat.
type Heartbeat struct {
	AgentID       string
	Status        string
	CurrentTask   *string
	LastCompleted *string
	NextStep      *string
	UpdatedAt     time.Time
}

// Get returns the heartbeat for the given agent, or nil if not found.
func (s *HeartbeatStore) Get(ctx context.Context, agentID string) (*Heartbeat, error) {
	var query string
	switch s.driverName {
	case "postgres":
		query = `SELECT agent_id, status, current_task, last_completed, next_step, updated_at FROM agent_heartbeat WHERE agent_id = $1`
	default:
		query = `SELECT agent_id, status, current_task, last_completed, next_step, updated_at FROM agent_heartbeat WHERE agent_id = ?`
	}

	h := &Heartbeat{}
	var updatedAtStr string
	err := s.db.QueryRowContext(ctx, query, agentID).Scan(
		&h.AgentID, &h.Status, &h.CurrentTask, &h.LastCompleted, &h.NextStep, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Parse updated_at — PostgreSQL returns time.Time natively via driver,
	// SQLite returns a string.
	if t, parseErr := time.Parse("2006-01-02 15:04:05", updatedAtStr); parseErr == nil {
		h.UpdatedAt = t
	} else if t, parseErr := time.Parse(time.RFC3339, updatedAtStr); parseErr == nil {
		h.UpdatedAt = t
	}

	return h, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
