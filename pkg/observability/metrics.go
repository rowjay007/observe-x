// Package observability holds the Prometheus collectors and helper
// HTTP handlers shared by every ObserveX service. Centralising them
// here keeps metric names / labels consistent across services and gives
// Phase C a single point to wire dashboards.
//
// Naming follows Prometheus conventions:
//   - `_total` suffix on monotonically increasing counters.
//   - `_seconds` units in histograms.
//   - `tenant` label cardinality is bounded by the number of paying
//     tenants — we explicitly accept this; tenant-level isolation
//     metrics are non-negotiable for an observability platform.
package observability

import (
	"net/http"
	"net/http/pprof"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SignalsReceived counts every signal the gateway accepted into the
// ingest channel. Labelled by tenant and signal type so dashboards
// can break down ingress per tenant.
var SignalsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "observex",
	Subsystem: "ingest",
	Name:      "signals_received_total",
	Help:      "Total signals accepted for processing.",
}, []string{"tenant", "type"})

// SignalsDropped counts signals that were rejected, sampled out, or
// failed at some stage. The `reason` label is one of:
//   overload, no_tenant, decode, actor_full, sampled_out, wal_error.
var SignalsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "observex",
	Subsystem: "ingest",
	Name:      "signals_dropped_total",
	Help:      "Total signals dropped, by tenant, type, and reason.",
}, []string{"tenant", "type", "reason"})

// WALWriteLatency tracks the wall-clock time to append a single entry
// to the WAL. Used to verify the Phase A SLO of <5ms P99.
var WALWriteLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Namespace: "observex",
	Subsystem: "wal",
	Name:      "write_seconds",
	Help:      "Latency of a single WAL append (group-commit excluded).",
	Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 12), // 100µs → ~410ms
})

// PipelineQueueDepth is the current occupancy of the bounded ingest
// channel. Alerts should fire when this approaches the channel size.
var PipelineQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "observex",
	Subsystem: "ingest",
	Name:      "pipeline_queue_depth",
	Help:      "Current number of signals queued in the ingest channel.",
})

// ClickHouseInflightBatches gauges the number of batches in-flight to
// ClickHouse. Useful for spotting circuit-breaker openings.
var ClickHouseInflightBatches = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: "observex",
	Subsystem: "clickhouse",
	Name:      "inflight_batches",
	Help:      "Number of in-flight batch INSERTs to ClickHouse.",
})

// MetricsHandler returns the standard Prometheus scrape handler.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// RegisterDebugHandlers mounts /debug/pprof/* onto the supplied mux.
// Callers MUST gate this behind an admin auth check or a separate
// listener — pprof endpoints leak heap dumps and goroutine traces.
func RegisterDebugHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
