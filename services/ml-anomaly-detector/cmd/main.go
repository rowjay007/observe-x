// ml-anomaly-detector is a Phase B-5 skeleton service. In Phase C it
// will subscribe to a NATS JetStream feed of metric signals; for now
// it exposes an HTTP `POST /v1/observations` endpoint that ingests
// (tenant, metric, value, timestamp) records and surfaces detected
// anomalies via Prometheus counters and an SSE stream.
//
// The detection algorithm is the rolling z-score implemented in
// internal/detector. Real seasonal models (STL, Prophet) land in
// Phase C; the abstraction (`Detector.Observe`) is stable so the
// algorithm can swap without API churn.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/selfobs"
	"github.com/rowjay007/observe-x/services/ml-anomaly-detector/internal/detector"
)

var (
	anomaliesFired = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "observex_anomalies_fired_total",
		Help: "Anomalies detected, labelled by tenant.",
	}, []string{"tenant"})
	observations = promauto.NewCounter(prometheus.CounterOpts{
		Name: "observex_anomaly_observations_total",
		Help: "Total observations ingested by the anomaly detector.",
	})
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	tp, _ := selfobs.InitFromEnv(context.Background(), "ml-anomaly-detector", "1.0.0")
	if tp != nil {
		defer func() {
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			defer cc()
			_ = tp.Shutdown(c)
		}()
	}

	d := detector.New(detector.Options{
		WarmupSamples: envInt("OBSERVE_X_ANOMALY_WARMUP", 50),
		ZThreshold:    envFloat("OBSERVE_X_ANOMALY_Z", 3.0),
		Alpha:         envFloat("OBSERVE_X_ANOMALY_ALPHA", 0.05),
	})

	var totalObs atomic.Int64

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ready", "series": d.SeriesCount()}) })
	r.GET("/metrics", gin.WrapH(observability.MetricsHandler()))

	r.POST("/v1/observations", func(c *gin.Context) {
		var req struct {
			TenantID  string  `json:"tenant_id"`
			Metric    string  `json:"metric"`
			Value     float64 `json:"value"`
			Timestamp string  `json:"timestamp,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.TenantID == "" || req.Metric == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id and metric are required"})
			return
		}
		at := time.Now().UTC()
		if req.Timestamp != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, req.Timestamp); err == nil {
				at = parsed
			}
		}
		observations.Inc()
		totalObs.Add(1)
		if anom := d.Observe(req.TenantID, req.Metric, req.Value, at); anom != nil {
			anomaliesFired.WithLabelValues(anom.TenantID).Inc()
			c.JSON(http.StatusOK, gin.H{
				"anomaly": map[string]any{
					"tenant_id": anom.TenantID,
					"metric":    anom.Metric,
					"value":     anom.Value,
					"z":         anom.ZScore,
					"mean":      anom.Mean,
					"threshold": anom.Threshold,
					"at":        anom.At.UTC().Format(time.RFC3339Nano),
				},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"anomaly": nil})
	})

	srv := &http.Server{
		Addr:         getEnv("OBSERVE_X_ANOMALY_ADDR", ":7600"),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("ml-anomaly-detector listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	logger.Info("ml-anomaly-detector stopped", zap.Int64("total_observations", totalObs.Load()))
}

// ─── env helpers ─────────────────────────────────────────────────────────

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return d
}

var _ = json.Marshal // keep symbol; gin uses it via reflection
