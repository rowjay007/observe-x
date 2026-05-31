package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/alertsink"
	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/selfobs"
	pkgsignal "github.com/rowjay007/observe-x/pkg/signal"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/otlp"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/receiver"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		// Logger setup precedes the logger, so stdlib is acceptable here.
		panic("ingest-gateway: zap init failed: " + err.Error())
	}
	defer func() { _ = logger.Sync() }()

	// Phase C-2: self-observability. The ingest-gateway exports its
	// own traces back through itself (loopback). Missing env vars ⇒
	// no-op provider; never a startup failure.
	tracerCtx := context.Background()
	tp, err := selfobs.InitFromEnv(tracerCtx, "ingest-gateway", "1.0.0")
	if err != nil {
		logger.Warn("selfobs init failed; tracing disabled", zap.Error(err))
	}
	if tp != nil {
		defer func() {
			ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = tp.Shutdown(ctx)
		}()
	}

	walPath := getEnv("OBSERVE_X_WAL_PATH", "/tmp/observex/wal")

	engineOpts := engine.Options{
		WALPath:       walPath,
		SamplingRate:  envFloat("OBSERVE_X_SAMPLING_RATE", 0.1),
		MaxTraceQueue: envInt("OBSERVE_X_TRACE_QUEUE", 1000),
		ClickHouse: &chstorage.Options{
			Addr:           getEnv("OBSERVE_X_CLICKHOUSE_ADDR", "localhost:9000"),
			Database:       getEnv("OBSERVE_X_CLICKHOUSE_DB", "observex"),
			Username:       os.Getenv("OBSERVE_X_CLICKHOUSE_USER"),
			Password:       os.Getenv("OBSERVE_X_CLICKHOUSE_PASSWORD"),
			MigrateOnStart: true,
		},
	}

	procEngine, err := engine.NewProcessingEngineWithOptions(engineOpts)
	if err != nil {
		logger.Fatal("engine init failed", zap.Error(err))
	}
	defer func() { _ = procEngine.Stop() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := procEngine.Start(ctx); err != nil {
		logger.Fatal("engine start failed", zap.Error(err))
	}

	keyStore, keyStoreClose, err := initKeyStore(ctx, logger)
	if err != nil {
		logger.Fatal("key store init", zap.Error(err))
	}
	defer keyStoreClose()
	authMW := auth.NewAuthMiddleware(keyStore)

	// Phase C-1: when the alert-manager is configured, every actor's
	// CEP events forward through this sink. If the env var is unset,
	// CEP events are dropped (loud counter, no notification).
	if amURL := os.Getenv("OBSERVE_X_ALERT_MANAGER_URL"); amURL != "" {
		sink := alertsink.NewHTTPSink(alertsink.HTTPOptions{
			Endpoint: amURL + "/v1/events",
			APIKey:   os.Getenv("OBSERVE_X_ALERT_MANAGER_API_KEY"),
		})
		defer sink.Stop()
		procEngine.SetAlertSink(sink)
		logger.Info("alert-manager wired", zap.String("url", amURL))
	}

	serverTLS, err := loadServerTLSConfig()
	if err != nil {
		logger.Fatal("tls config", zap.Error(err))
	}

	// ── HTTP server (4318) ────────────────────────────────────────────
	router := buildRouter(authMW, procEngine, logger)
	httpServer := &http.Server{
		Addr:         getEnv("OBSERVE_X_HTTP_ADDR", ":4318"),
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── gRPC server (4317) ────────────────────────────────────────────
	grpcReceiver := receiver.NewGRPCReceiver(
		getEnv("OBSERVE_X_GRPC_ADDR", ":4317"),
		procEngine, keyStore, logger, serverTLS,
	)

	// ── StatsD UDP (8125) ─────────────────────────────────────────────
	statsdReceiver := receiver.NewStatsDReceiver(
		getEnv("OBSERVE_X_STATSD_ADDR", ":8125"),
		procEngine,
		getEnv("OBSERVE_X_DEFAULT_TENANT", "default"),
		logger,
	)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		logger.Info("HTTP listener", zap.String("addr", httpServer.Addr), zap.Bool("tls", serverTLS != nil))
		if err := serveHTTP(httpServer, serverTLS); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := grpcReceiver.Start(ctx); err != nil {
			logger.Error("grpc receiver", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := statsdReceiver.Start(ctx); err != nil {
			logger.Error("statsd receiver", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutdown requested")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", zap.Error(err))
	}
	grpcReceiver.Stop()
	statsdReceiver.Stop()
	wg.Wait()
	logger.Info("shutdown complete")
}

// buildRouter wires the gin router used by the HTTP listener. It is
// exported indirectly via tests in this package.
func buildRouter(authMW *auth.AuthMiddleware, procEngine *engine.ProcessingEngine, logger *zap.Logger) *gin.Engine {
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/ready", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// Metrics endpoint must be unauthenticated for scrapers (NetworkPolicy
	// limits who can reach it in Phase C).
	r.GET("/metrics", gin.WrapH(observability.MetricsHandler()))

	// pprof gated by env flag — never on by default in production.
	if os.Getenv("OBSERVE_X_PPROF_ENABLED") == "true" {
		mux := http.NewServeMux()
		observability.RegisterDebugHandlers(mux)
		r.Any("/debug/pprof/*any", gin.WrapH(mux))
	}

	authorized := r.Group("/")
	authorized.Use(ginAuth(authMW))
	authorized.POST("/v1/ingest", ingestHandler(procEngine, logger))

	otlpHandler := otlp.NewHandler(procEngine)
	authorized.POST("/v1/traces", gin.WrapF(otlpHandler.HandleTraces))
	authorized.POST("/v1/metrics", gin.WrapF(otlpHandler.HandleMetrics))
	authorized.POST("/v1/logs", gin.WrapF(otlpHandler.HandleLogs))

	// Backward-compat alias for Phase A callers still hitting /v1/otlp/traces.
	authorized.POST("/v1/otlp/traces", gin.WrapF(otlpHandler.HandleTraces))

	return r
}

func ginAuth(mw *auth.AuthMiddleware) gin.HandlerFunc {
	return func(c *gin.Context) {
		mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
		if c.Writer.Written() {
			c.Abort()
		}
	}
}

func ingestHandler(procEngine *engine.ProcessingEngine, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			TenantID string            `json:"tenant_id"`
			Type     pkgsignal.Type    `json:"type"`
			Payload  []byte            `json:"payload"`
			Attrs    map[string]string `json:"attributes"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		if req.TenantID != "" && req.TenantID != tenantID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tenant id mismatch"})
			return
		}
		sig := pkgsignal.Signal{
			TenantID:   tenantID,
			Type:       req.Type,
			Payload:    req.Payload,
			Attributes: req.Attrs,
			ReceivedAt: time.Now().UTC(),
		}
		if err := procEngine.ProcessSignal(c.Request.Context(), sig); err != nil {
			logger.Warn("backpressure", zap.String("tenant", tenantID), zap.Error(err))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "overloaded, retry later"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
	}
}

func serveHTTP(server *http.Server, tlsConfig *tls.Config) error {
	if tlsConfig == nil {
		return server.ListenAndServe()
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	return server.Serve(tls.NewListener(listener, tlsConfig))
}

func loadServerTLSConfig() (*tls.Config, error) {
	certFile := os.Getenv("OBSERVE_X_TLS_CERT_FILE")
	keyFile := os.Getenv("OBSERVE_X_TLS_KEY_FILE")
	caFile := os.Getenv("OBSERVE_X_TLS_CA_FILE")
	if certFile == "" && keyFile == "" && caFile == "" {
		return nil, nil
	}
	cfg := &auth.TLSConfig{CertFile: certFile, KeyFile: keyFile, CAFile: caFile}
	return cfg.LoadServerConfig()
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// initKeyStore selects the production PostgresKeyStore when
// OBSERVE_X_POSTGRES_URL is set; otherwise it falls back to the
// dev-only StatelessKeyValidator (with a loud warning). The returned
// close func MUST be deferred by the caller.
func initKeyStore(ctx context.Context, logger *zap.Logger) (auth.KeyStore, func(), error) {
	if dsn := os.Getenv("OBSERVE_X_POSTGRES_URL"); dsn != "" {
		store, err := auth.NewPostgresKeyStore(ctx, dsn, auth.PostgresOptions{})
		if err != nil {
			return nil, func() {}, err
		}
		logger.Info("auth: PostgresKeyStore active")
		return store, func() { _ = store.Close() }, nil
	}

	apiSecret, ok := os.LookupEnv("OBSERVE_X_API_SECRET")
	if !ok || apiSecret == "" {
		return nil, func() {}, errors.New(
			"either OBSERVE_X_POSTGRES_URL (production) or " +
				"OBSERVE_X_API_SECRET (dev) MUST be set")
	}
	logger.Warn("auth: StatelessKeyValidator active (DEV ONLY); set OBSERVE_X_POSTGRES_URL for production")
	return auth.NewStatelessKeyValidator(apiSecret), func() {}, nil
}
