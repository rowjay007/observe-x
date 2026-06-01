// cold-tier-controller is ObserveX's storage-tier observer.
//
// It periodically queries ClickHouse `system.parts` for every
// ObserveX-owned table (metrics / logs / traces), groups the result
// by (table, disk_name), and exposes a Prometheus gauge so operators
// can:
//
//   - see whether the cold-tier S3 move is actually moving parts,
//   - alarm when the hot disk grows beyond a threshold,
//   - alarm when the cold-disk PUT/GET pipeline stalls (parts
//     remain on hot well past the TTL boundary).
//
// We do NOT trigger moves ourselves — ClickHouse's background merger
// does that on the TTL schedule. This service is read-only against
// ClickHouse. The "controller" naming nods to its operational role,
// not to a write surface.
//
// Self-observability: full pkg/selfobs integration; OTLP traces and
// Prometheus metrics flow through the same loopback as every other
// ObserveX service.
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/selfobs"
)

var (
	partsGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "observex_clickhouse_parts",
		Help: "Number of ClickHouse parts per (table, disk).",
	}, []string{"table", "disk"})

	bytesGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "observex_clickhouse_bytes",
		Help: "Disk bytes per (table, disk).",
	}, []string{"table", "disk"})

	scrapeLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "observex_cold_tier_scrape_seconds",
		Help:    "Latency of the cold-tier system.parts scrape.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	})
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	tp, _ := selfobs.InitFromEnv(context.Background(), "cold-tier-controller", "1.0.0")
	if tp != nil {
		defer func() {
			ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = tp.Shutdown(ctx)
		}()
	}

	addr := getEnv("OBSERVE_X_CLICKHOUSE_ADDR", "localhost:9000")
	db := getEnv("OBSERVE_X_CLICKHOUSE_DB", "observex")
	httpAddr := getEnv("OBSERVE_X_COLD_TIER_ADDR", ":7800")
	interval, _ := time.ParseDuration(getEnv("OBSERVE_X_COLD_TIER_INTERVAL", "60s"))
	if interval < 10*time.Second {
		interval = 10 * time.Second // floor — system.parts is not free
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: db,
			Username: getEnv("OBSERVE_X_CLICKHOUSE_USER", "default"),
			Password: os.Getenv("OBSERVE_X_CLICKHOUSE_PASS"),
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		logger.Fatal("clickhouse open", zap.Error(err))
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(context.Background()); err != nil {
		logger.Fatal("clickhouse ping", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scrapeLoop(ctx, conn, db, interval, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.Handle("/metrics", observability.MetricsHandler())
	mux.Handle("/metrics-raw", promhttp.Handler())

	srv := &http.Server{
		Addr:         httpAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		logger.Info("cold-tier-controller listening",
			zap.String("addr", httpAddr),
			zap.Duration("scrape_interval", interval))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	shutdownCtx, cc := context.WithTimeout(context.Background(), 10*time.Second)
	defer cc()
	_ = srv.Shutdown(shutdownCtx)
}

func scrapeLoop(ctx context.Context, conn clickhouse.Conn, db string, every time.Duration, logger *zap.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	// First scrape immediately so /metrics has data on the first
	// request rather than after one full interval.
	scrapeOnce(ctx, conn, db, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scrapeOnce(ctx, conn, db, logger)
		}
	}
}

// scrapeOnce queries system.parts and updates the gauges. Failure is
// logged but never fatal — a transient ClickHouse hiccup must not
// take the controller pod into a CrashLoopBackOff.
func scrapeOnce(ctx context.Context, conn clickhouse.Conn, db string, logger *zap.Logger) {
	start := time.Now()
	defer func() { scrapeLatency.Observe(time.Since(start).Seconds()) }()

	scrapeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := conn.Query(scrapeCtx, `
		SELECT table, disk_name, count() AS parts, sum(bytes_on_disk) AS bytes
		FROM system.parts
		WHERE database = ? AND active = 1
		GROUP BY table, disk_name
	`, db)
	if err != nil {
		logger.Warn("system.parts scrape", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	// Snapshot first, then reset + apply so the gauges stay consistent.
	type row struct {
		Table string
		Disk  string
		Parts uint64
		Bytes uint64
	}
	var snap []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.Table, &r.Disk, &r.Parts, &r.Bytes); err != nil {
			logger.Warn("scan", zap.Error(err))
			continue
		}
		snap = append(snap, r)
	}
	partsGauge.Reset()
	bytesGauge.Reset()
	for _, r := range snap {
		partsGauge.WithLabelValues(r.Table, r.Disk).Set(float64(r.Parts))
		bytesGauge.WithLabelValues(r.Table, r.Disk).Set(float64(r.Bytes))
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
