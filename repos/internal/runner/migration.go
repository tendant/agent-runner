package runner

import (
	"context"
	"database/sql"
	"fmt"
)

const postgresHeartbeatDDL = `
CREATE TABLE IF NOT EXISTS agent_heartbeat (
    agent_id       TEXT PRIMARY KEY,
    status         TEXT NOT NULL DEFAULT 'idle',
    current_task   TEXT,
    last_completed TEXT,
    next_step      TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- NOTIFY trigger on workflow_run INSERT for agent.% types
CREATE OR REPLACE FUNCTION notify_agent_wake() RETURNS trigger AS $$
BEGIN
    IF NEW.type LIKE 'agent.%' THEN
        PERFORM pg_notify('agent_wake', NEW.id::text);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger WHERE tgname = 'trg_agent_wake'
    ) THEN
        CREATE TRIGGER trg_agent_wake
            AFTER INSERT ON workflow_run
            FOR EACH ROW
            EXECUTE FUNCTION notify_agent_wake();
    END IF;
END;
$$;
`

const sqliteHeartbeatDDL = `
CREATE TABLE IF NOT EXISTS agent_heartbeat (
    agent_id       TEXT PRIMARY KEY,
    status         TEXT NOT NULL DEFAULT 'idle',
    current_task   TEXT,
    last_completed TEXT,
    next_step      TEXT,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// AutoMigrate creates the agent_heartbeat table and (on PostgreSQL) the NOTIFY trigger.
func AutoMigrate(ctx context.Context, db *sql.DB, driverName string) error {
	var ddl string
	switch driverName {
	case "postgres":
		ddl = postgresHeartbeatDDL
	case "sqlite":
		ddl = sqliteHeartbeatDDL
	default:
		return fmt.Errorf("unsupported driver: %s", driverName)
	}

	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("runner auto-migrate failed: %w", err)
	}
	return nil
}
