package runner

import (
	"database/sql"
	"fmt"
)

// DebugDB provides read-only queries for debugging the schedule/run tables.
type DebugDB struct {
	db         *sql.DB
	driverName string
}

// NewDebugDB creates a DebugDB from the runner's database.
func NewDebugDB(db *sql.DB, driverName string) *DebugDB {
	return &DebugDB{db: db, driverName: driverName}
}

// QuerySchedules returns all active schedules from workflow_schedule.
func (d *DebugDB) QuerySchedules() ([]map[string]interface{}, error) {
	rows, err := d.db.Query(`
		SELECT id, type, payload, schedule, timezone, next_run_at, last_run_at, enabled, created_at
		FROM workflow_schedule
		WHERE deleted_at IS NULL
		ORDER BY next_run_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query schedules: %w", err)
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, typ, payload, schedule, timezone string
		var nextRunAt, createdAt string
		var lastRunAt sql.NullString
		var enabled bool

		if err := rows.Scan(&id, &typ, &payload, &schedule, &timezone, &nextRunAt, &lastRunAt, &enabled, &createdAt); err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}

		row := map[string]interface{}{
			"id":          id,
			"type":        typ,
			"payload":     payload,
			"schedule":    schedule,
			"timezone":    timezone,
			"next_run_at": nextRunAt,
			"enabled":     enabled,
			"created_at":  createdAt,
		}
		if lastRunAt.Valid {
			row["last_run_at"] = lastRunAt.String
		}
		results = append(results, row)
	}

	return results, nil
}

// QueryRuns returns the most recent workflow runs.
func (d *DebugDB) QueryRuns(limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.db.Query(`
		SELECT id, type, payload, status, priority, run_at, attempt, max_attempts,
		       leased_by, lease_until, last_error, created_at, updated_at
		FROM workflow_run
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, typ, payload, status string
		var priority, attempt, maxAttempts int
		var runAt, createdAt, updatedAt string
		var leasedBy, leaseUntil, lastError sql.NullString

		if err := rows.Scan(&id, &typ, &payload, &status, &priority, &runAt,
			&attempt, &maxAttempts, &leasedBy, &leaseUntil, &lastError,
			&createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}

		row := map[string]interface{}{
			"id":           id,
			"type":         typ,
			"payload":      payload,
			"status":       status,
			"priority":     priority,
			"run_at":       runAt,
			"attempt":      attempt,
			"max_attempts": maxAttempts,
			"created_at":   createdAt,
			"updated_at":   updatedAt,
		}
		if leasedBy.Valid {
			row["leased_by"] = leasedBy.String
		}
		if leaseUntil.Valid {
			row["lease_until"] = leaseUntil.String
		}
		if lastError.Valid {
			row["last_error"] = lastError.String
		}
		results = append(results, row)
	}

	return results, nil
}
