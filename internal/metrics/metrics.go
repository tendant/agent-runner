package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	SessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_sessions_total",
		Help: "Total number of agent sessions by status and source.",
	}, []string{"status", "source"})

	IterationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_iterations_total",
		Help: "Total number of agent iterations by status and source.",
	}, []string{"status", "source"})

	ActiveSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agent_active_sessions",
		Help: "Number of currently running agent sessions by source.",
	}, []string{"source"})

	CostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_cost_usd_total",
		Help: "Total cost in USD across all agent iterations by source.",
	}, []string{"source"})

	IterationDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agent_iteration_duration_seconds",
		Help:    "Duration of individual agent iterations in seconds by source.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~4096s
	}, []string{"source"})
)
