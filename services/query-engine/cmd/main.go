// query-engine is the ObserveX read path. It accepts ObserveQL
// queries from authenticated tenants, lowers them to ClickHouse SQL
// via pkg/observeql, executes against the same ClickHouse cluster
// that the ingest-gateway writes to, and streams the results as
// NDJSON.
//
// Auth: the same pkg/auth.PostgresKeyStore that fronts the
// ingest-gateway also fronts this service. A tenant's API key is its
// query credential; no separate read scope exists in Phase B-3
// (scopes land in Phase C).
//
// Tenant safety: pkg/observeql.PlanQuery always injects
// `tenant_id = ?` with the trusted, auth-derived tenant id as the
// first parameter. Caller-supplied WHERE clauses can refer to
// tenant_id but cannot override it (see TestPlanRejectsCallerSuppliedTenant).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/observeql"
	"github.com/rowjay007/observe-x/pkg/selfobs"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
	"github.com/rowjay007/observe-x/services/query-engine/internal/executor"
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	tp, err := selfobs.InitFromEnv(context.Background(), "query-engine", "1.0.0")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── ClickHouse client (query-side; same cluster, read-only path).
	chClient, err := chstorage.NewClient(chstorage.Options{
		Addr:         getEnv("OBSERVE_X_CLICKHOUSE_ADDR", "localhost:9000"),
		Database:     getEnv("OBSERVE_X_CLICKHOUSE_DB", "observex"),
		Username:     os.Getenv("OBSERVE_X_CLICKHOUSE_USER"),
		Password:     os.Getenv("OBSERVE_X_CLICKHOUSE_PASSWORD"),
		DialTimeout:  5 * time.Second,
		MaxOpenConns: 16,
	})
	if err != nil {
		logger.Fatal("clickhouse init", zap.Error(err))
	}
	defer func() { _ = chClient.Close() }()

	exec := executor.New(chClient)

	// ── KeyStore: same Postgres as the gateway.
	keyStore, keyStoreClose, err := initKeyStore(ctx, logger)
	if err != nil {
		logger.Fatal("auth init", zap.Error(err))
	}
	defer keyStoreClose()
	authMW := auth.NewAuthMiddleware(keyStore)

	router := buildRouter(authMW, exec, logger)

	httpServer := &http.Server{
		Addr:         getEnv("OBSERVE_X_QUERY_ADDR", ":7500"),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // queries can stream a while
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("query-engine listening", zap.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutdown requested")

	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("query-engine stopped")
}

// ─── Router ──────────────────────────────────────────────────────────────

func buildRouter(authMW *auth.AuthMiddleware, exec *executor.Executor, logger *zap.Logger) http.Handler {
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ready"}) })
	r.GET("/metrics", gin.WrapH(observability.MetricsHandler()))

	authorized := r.Group("/")
	authorized.Use(ginAuth(authMW))
	// Phase C-3a: query endpoint requires the explicit `query` scope.
	// An ingest-only key (the default) cannot read data.
	authorized.POST("/v1/query", auth.GinRequireScope(auth.ScopeQuery), queryHandler(exec, logger))

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

// ─── Query handler ───────────────────────────────────────────────────────

type queryReq struct {
	Query        string `json:"query"`
	MaxRows      int    `json:"max_rows,omitempty"`
	TimeoutSecs  int    `json:"timeout_secs,omitempty"`
}

func queryHandler(exec *executor.Executor, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}

		var req queryReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
			return
		}
		if req.Query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
			return
		}

		ast, err := observeql.Parse(req.Query)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		plan, err := observeql.PlanQuery(ast, observeql.PlannerOptions{
			TenantID:    tenantID,
			MaxRowLimit: req.MaxRows,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		timeout := 30 * time.Second
		if req.TimeoutSecs > 0 && req.TimeoutSecs < 300 {
			timeout = time.Duration(req.TimeoutSecs) * time.Second
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()

		c.Writer.Header().Set("Content-Type", "application/x-ndjson")
		c.Writer.WriteHeader(http.StatusOK)

		if _, err := exec.Execute(ctx, plan, c.Writer); err != nil {
			// Headers are already sent so we can't switch to JSON 500; emit
			// a final NDJSON error record instead. The client should detect
			// _kind=error in its trailer parse.
			logger.Warn("query execute", zap.String("tenant", tenantID), zap.Error(err))
			_ = json.NewEncoder(c.Writer).Encode(map[string]any{
				"_kind": "error",
				"error": err.Error(),
			})
		}
	}
}

// ─── env helpers / KeyStore wiring (same shape as ingest-gateway) ───────

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
			"either OBSERVE_X_POSTGRES_URL (production) or OBSERVE_X_API_SECRET (dev) MUST be set")
	}
	logger.Warn("auth: StatelessKeyValidator active (DEV ONLY)")
	return auth.NewStatelessKeyValidator(apiSecret), func() {}, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

var _ = strconv.Atoi
