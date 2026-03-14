package runner

import (
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// Listener wraps pq.Listener for PostgreSQL LISTEN/NOTIFY.
// On SQLite, NewListener returns nil and the runner skips NOTIFY setup.
type Listener struct {
	pql    *pq.Listener
	wakeCh chan struct{}
	stopCh chan struct{}
}

// NewListener creates a PostgreSQL LISTEN/NOTIFY listener.
// Returns nil if driverName is not "postgres".
func NewListener(driverName, connString string) *Listener {
	if driverName != "postgres" {
		return nil
	}

	wakeCh := make(chan struct{}, 1)
	stopCh := make(chan struct{})

	reportProblem := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			slog.Warn("runner listener event", "event_type", ev, "error", err)
		}
	}

	pql := pq.NewListener(connString, 10*time.Second, 60*time.Second, reportProblem)
	if err := pql.Listen("agent_wake"); err != nil {
		slog.Error("runner listener: failed to LISTEN on agent_wake", "error", err)
		pql.Close()
		return nil
	}

	l := &Listener{
		pql:    pql,
		wakeCh: wakeCh,
		stopCh: stopCh,
	}

	go l.loop()
	return l
}

// Wake returns a channel that fires when a NOTIFY is received.
func (l *Listener) Wake() <-chan struct{} {
	if l == nil {
		// Return a channel that never fires (SQLite path)
		return make(chan struct{})
	}
	return l.wakeCh
}

// Close stops the listener and releases resources.
func (l *Listener) Close() {
	if l == nil {
		return
	}
	select {
	case <-l.stopCh:
	default:
		close(l.stopCh)
	}
	l.pql.Close()
}

func (l *Listener) loop() {
	keepalive := time.NewTicker(90 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-l.pql.Notify:
			// Non-blocking send
			select {
			case l.wakeCh <- struct{}{}:
			default:
			}
		case <-keepalive.C:
			l.pql.Ping()
		}
	}
}
