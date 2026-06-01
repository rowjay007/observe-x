// tenant-api is the ObserveX control-plane HTTP service. It owns the
// tenants and tenant_api_keys tables in Postgres and is the only
// component that writes to them. The ingest-gateway is a read-only
// consumer via pkg/auth.PostgresKeyStore.
//
// All admin endpoints require a bootstrap admin token (env-configured)
// in Phase B-1. Phase B-3+ will replace it with operator OIDC.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/rowjay007/observe-x/pkg/auditlog"
	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/oidc"
	"github.com/rowjay007/observe-x/pkg/retention"
	"github.com/rowjay007/observe-x/pkg/selfobs"
	"github.com/rowjay007/observe-x/services/tenant-api/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	tp, _ := selfobs.InitFromEnv(context.Background(), "tenant-api", "1.0.0")
	if tp != nil {
		defer func() {
			ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = tp.Shutdown(ctx)
		}()
	}

	dsn := mustEnv(logger, "OBSERVE_X_POSTGRES_URL")
	// Phase C-3b: OIDC is the production auth path; admin-token is
	// the break-glass fallback. Exactly one of them MUST be set.
	// If both are present, we fail closed to remove the dual-path
	// attack surface.
	issuer := os.Getenv("OBSERVE_X_OIDC_ISSUER")
	adminToken := os.Getenv("OBSERVE_X_TENANT_API_ADMIN_TOKEN")
	if issuer == "" && adminToken == "" {
		logger.Fatal("either OBSERVE_X_OIDC_ISSUER or OBSERVE_X_TENANT_API_ADMIN_TOKEN must be set")
	}
	if issuer != "" && adminToken != "" {
		logger.Fatal("OIDC and admin-token are mutually exclusive; unset OBSERVE_X_TENANT_API_ADMIN_TOKEN once OIDC is verified")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	keyStore, err := auth.NewPostgresKeyStore(ctx, dsn, auth.PostgresOptions{})
	if err != nil {
		logger.Fatal("postgres key store init", zap.Error(err))
	}
	defer func() { _ = keyStore.Close() }()

	if err := store.ApplyMigrations(ctx, keyStore.Pool()); err != nil {
		logger.Fatal("migrations", zap.Error(err))
	}
	repo := store.New(keyStore.Pool())

	auditExp, auditCloser := buildAuditExporter(ctx, logger)
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := auditCloser(shutdownCtx); err != nil {
			logger.Warn("audit-log close", zap.Error(err))
		}
	}()

	var oidcVal *oidc.Validator
	if issuer != "" {
		audience := getEnv("OBSERVE_X_OIDC_AUDIENCE", "observex")
		adminGroups := splitCSV(os.Getenv("OBSERVE_X_OIDC_ADMIN_GROUPS"))
		groupClaim := getEnv("OBSERVE_X_OIDC_GROUP_CLAIM", "groups")
		v, err := oidc.NewValidator(ctx, oidc.Config{
			Issuer:      issuer,
			Audience:    audience,
			AdminGroups: adminGroups,
			GroupClaim:  groupClaim,
		})
		if err != nil {
			logger.Fatal("oidc validator init", zap.Error(err))
		}
		defer v.Close()
		oidcVal = v
		logger.Info("operator OIDC active",
			zap.String("issuer", issuer),
			zap.String("audience", audience),
			zap.Strings("admin_groups", adminGroups))
	}

	// Phase D-2: optional ClickHouse connection for per-tenant
	// retention overrides (ADR-0019). When unset, the PUT
	// /v1/tenants/:id/retention endpoint returns 501 Not
	// Implemented; tenant CRUD stays fully functional.
	var chConn driver.Conn
	if addr := os.Getenv("OBSERVE_X_CLICKHOUSE_ADDR"); addr != "" {
		c, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{addr},
			Auth: clickhouse.Auth{
				Database: getEnv("OBSERVE_X_CLICKHOUSE_DB", "observex"),
				Username: getEnv("OBSERVE_X_CLICKHOUSE_USER", "default"),
				Password: os.Getenv("OBSERVE_X_CLICKHOUSE_PASS"),
			},
			DialTimeout: 5 * time.Second,
		})
		if err != nil {
			logger.Warn("clickhouse: open failed; retention API will return 501",
				zap.Error(err))
		} else {
			chConn = c
			defer func() { _ = c.Close() }()
		}
	}

	srv := &server{
		logger:     logger,
		adminToken: adminToken,
		oidc:       oidcVal,
		repo:       repo,
		keyStore:   keyStore,
		auditExp:   auditExp,
		chConn:     chConn,
	}

	router := srv.router()
	httpServer := &http.Server{
		Addr:         getEnv("OBSERVE_X_TENANT_API_ADDR", ":7400"),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("tenant-api listening", zap.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("tenant-api stopped")
}

// ─── server / router ─────────────────────────────────────────────────────

type server struct {
	logger     *zap.Logger
	adminToken string
	oidc       *oidc.Validator // nil when admin-token break-glass mode is active
	repo       *store.Store
	keyStore   *auth.PostgresKeyStore
	auditExp   auditlog.Exporter
	chConn     driver.Conn // nil ⇒ retention endpoint returns 501
}

func (s *server) router() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ready"}) })
	r.GET("/metrics", gin.WrapH(observability.MetricsHandler()))

	admin := r.Group("/v1")
	admin.Use(s.requireAdmin())
	{
		admin.POST("/tenants", s.createTenant)
		admin.GET("/tenants", s.listTenants)
		admin.GET("/tenants/:id", s.getTenant)
		admin.DELETE("/tenants/:id", s.deleteTenant)

		admin.GET("/tenants/:id/api-keys", s.listAPIKeys)
		admin.POST("/tenants/:id/api-keys", s.issueAPIKey)
		admin.DELETE("/tenants/:id/api-keys/:kid", s.revokeAPIKey)

		// Phase D-2: per-tenant retention overrides (ADR-0019).
		admin.PUT("/tenants/:id/retention", s.putRetention)
		admin.DELETE("/tenants/:id/retention", s.dropRetention)

		// Phase D-4: read-only audit log (ADR-0021).
		admin.GET("/audit", s.listAudit)
	}
	return r
}

// ─── admin auth ──────────────────────────────────────────────────────────

func (s *server) requireAdmin() gin.HandlerFunc {
	// OIDC mode: validator's gin adapter handles the bearer; the
	// stashed claims drive audit attribution downstream.
	if s.oidc != nil {
		return s.oidc.Gin()
	}
	// Break-glass: constant-time-compared static token.
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const bearer = "Bearer "
		if len(header) <= len(bearer) || header[:len(bearer)] != bearer {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing admin token"})
			return
		}
		got := header[len(bearer):]
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid admin token"})
			return
		}
		c.Next()
	}
}

// ─── handlers: tenants ───────────────────────────────────────────────────

type createTenantReq struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Tier          string `json:"tier,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
	QuotaEPS      int    `json:"quota_eps,omitempty"`
}

func (s *server) createTenant(c *gin.Context) {
	var req createTenantReq
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.ID == "" || req.DisplayName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id and display_name are required"})
		return
	}
	t := store.Tenant{
		ID:            req.ID,
		DisplayName:   req.DisplayName,
		Tier:          orDefault(req.Tier, "free"),
		RetentionDays: ifZero(req.RetentionDays, 14),
		QuotaEPS:      ifZero(req.QuotaEPS, 1000),
	}
	out, err := s.repo.CreateTenant(c.Request.Context(), t)
	if errors.Is(err, store.ErrTenantExists) {
		c.JSON(http.StatusConflict, gin.H{"error": "tenant already exists"})
		return
	}
	if err != nil {
		s.logger.Error("create tenant", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
		return
	}
	s.audit(c, &out.ID, "admin", "tenant.create", map[string]any{"tier": out.Tier})
	c.JSON(http.StatusCreated, tenantPayload(out))
}

func (s *server) getTenant(c *gin.Context) {
	t, err := s.repo.GetTenant(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrTenantNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	c.JSON(http.StatusOK, tenantPayload(t))
}

func (s *server) listTenants(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	ts, err := s.repo.ListTenants(c.Request.Context(), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, tenantPayload(t))
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "count": len(out)})
}

func (s *server) deleteTenant(c *gin.Context) {
	id := c.Param("id")
	err := s.repo.SoftDeleteTenant(c.Request.Context(), id)
	if errors.Is(err, store.ErrTenantNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	s.audit(c, &id, "admin", "tenant.delete", nil)
	c.Status(http.StatusNoContent)
}

// ─── handlers: api keys ──────────────────────────────────────────────────

type issueKeyReq struct {
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Scopes is the canonical scope set baked into the key. An empty
	// list falls back to auth.DefaultScopes() (ingest only). Unknown
	// scopes return 400 with the offending value.
	Scopes []string `json:"scopes,omitempty"`
}

func (s *server) issueAPIKey(c *gin.Context) {
	tenantID := c.Param("id")
	if _, err := s.repo.GetTenant(c.Request.Context(), tenantID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	var req issueKeyReq
	_ = c.BindJSON(&req) // body is optional

	scopes, err := auth.ParseScopes(req.Scopes)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	issued, err := s.keyStore.IssueKeyWithScopes(c.Request.Context(), tenantID, scopes, req.ExpiresAt)
	if err != nil {
		s.logger.Error("issue key", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue failed"})
		return
	}
	s.audit(c, &tenantID, "admin", "api_key.issue", map[string]any{
		"kid":    issued.KID,
		"prefix": issued.Prefix,
		"scopes": auth.ScopesAsStrings(issued.Scopes),
	})
	c.JSON(http.StatusCreated, map[string]any{
		"kid":        issued.KID,
		"raw_key":    issued.Raw, // show ONCE
		"prefix":     issued.Prefix,
		"scopes":     auth.ScopesAsStrings(issued.Scopes),
		"created_at": issued.CreatedAt,
		"expires_at": issued.ExpiresAt,
		"warning":    "raw_key is shown ONCE; store it securely. It will not be retrievable later.",
	})
}

func (s *server) listAPIKeys(c *gin.Context) {
	tenantID := c.Param("id")
	keys, err := s.repo.ListKeys(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": keys})
}

func (s *server) revokeAPIKey(c *gin.Context) {
	tenantID := c.Param("id")
	kid := c.Param("kid")
	err := s.keyStore.RevokeKey(c.Request.Context(), tenantID, kid)
	if errors.Is(err, auth.ErrKeyRevoked) {
		c.JSON(http.StatusGone, gin.H{"error": "already revoked"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke failed"})
		return
	}
	s.audit(c, &tenantID, "admin", "api_key.revoke", map[string]any{"kid": kid})
	c.Status(http.StatusNoContent)
}

// ─── handlers: audit (Phase D-4, ADR-0021) ───────────────────────────────

func (s *server) listAudit(c *gin.Context) {
	tenantID := c.Query("tenant_id")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	records, err := s.repo.ListAudit(c.Request.Context(), tenantID, limit)
	if err != nil {
		s.logger.Warn("audit list", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"records": records})
}

// ─── handlers: retention (Phase D-2, ADR-0019) ───────────────────────────

type retentionReq struct {
	MetricsHotDays   int `json:"metrics_hot_days"`
	MetricsTotalDays int `json:"metrics_total_days"`
	LogsHotDays      int `json:"logs_hot_days"`
	LogsTotalDays    int `json:"logs_total_days"`
	TracesHotDays    int `json:"traces_hot_days"`
	TracesTotalDays  int `json:"traces_total_days"`
}

func (s *server) putRetention(c *gin.Context) {
	if s.chConn == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "retention API requires OBSERVE_X_CLICKHOUSE_ADDR on tenant-api",
		})
		return
	}
	tenantID := c.Param("id")
	// Cross-check the tenant exists before issuing DDL — fail fast
	// with 404 rather than emitting an ALTER for a phantom tenant.
	if _, err := s.repo.GetTenant(c.Request.Context(), tenantID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	var req retentionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	spec := retention.Spec{
		TenantID:         tenantID,
		MetricsHotDays:   req.MetricsHotDays,
		MetricsTotalDays: req.MetricsTotalDays,
		LogsHotDays:      req.LogsHotDays,
		LogsTotalDays:    req.LogsTotalDays,
		TracesHotDays:    req.TracesHotDays,
		TracesTotalDays:  req.TracesTotalDays,
	}
	if err := retention.Apply(c.Request.Context(), s.chConn, spec); err != nil {
		s.logger.Warn("retention apply", zap.String("tenant", tenantID), zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, &tenantID, "admin", "retention.update", map[string]any{
		"metrics_hot_days": req.MetricsHotDays, "metrics_total_days": req.MetricsTotalDays,
		"logs_hot_days": req.LogsHotDays, "logs_total_days": req.LogsTotalDays,
		"traces_hot_days": req.TracesHotDays, "traces_total_days": req.TracesTotalDays,
	})
	c.JSON(http.StatusOK, gin.H{"status": "applied", "tenant_id": tenantID})
}

func (s *server) dropRetention(c *gin.Context) {
	if s.chConn == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "retention API requires OBSERVE_X_CLICKHOUSE_ADDR on tenant-api",
		})
		return
	}
	tenantID := c.Param("id")
	if err := retention.Drop(c.Request.Context(), s.chConn, tenantID); err != nil {
		s.logger.Warn("retention drop", zap.String("tenant", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.audit(c, &tenantID, "admin", "retention.drop", nil)
	c.Status(http.StatusNoContent)
}

// ─── helpers ─────────────────────────────────────────────────────────────

func (s *server) audit(c *gin.Context, tenantID *string, actor, action string, details map[string]any) {
	// Phase C-3b: when OIDC is active, the validated operator's
	// subject (and email when present) override the static "admin"
	// label so audit records carry the real principal.
	if s.oidc != nil {
		if sub := c.Request.Header.Get("X-Operator-Subject"); sub != "" {
			actor = sub
			if email := c.Request.Header.Get("X-Operator-Email"); email != "" {
				if details == nil {
					details = map[string]any{}
				}
				details["actor_email"] = email
			}
		}
	}
	srcIP := c.ClientIP()
	ev := store.AuditEvent{
		TenantID: tenantID,
		Actor:    actor,
		Action:   action,
		Details:  details,
		SourceIP: &srcIP,
	}
	if err := s.repo.WriteAudit(c.Request.Context(), ev); err != nil {
		s.logger.Warn("audit write failed", zap.Error(err), zap.String("action", action))
	}
	if s.auditExp != nil {
		tid := ""
		if tenantID != nil {
			tid = *tenantID
		}
		rec := auditlog.Record{
			TenantID:   tid,
			Actor:      actor,
			Action:     action,
			Details:    details,
			SourceIP:   srcIP,
			OccurredAt: time.Now().UTC(),
		}
		if err := s.auditExp.Append(c.Request.Context(), rec); err != nil {
			s.logger.Warn("audit-log export failed",
				zap.Error(err), zap.String("action", action))
		}
	}
}

func tenantPayload(t store.Tenant) map[string]any {
	out := map[string]any{
		"id":             t.ID,
		"display_name":   t.DisplayName,
		"tier":           t.Tier,
		"retention_days": t.RetentionDays,
		"quota_eps":      t.QuotaEPS,
		"created_at":     t.CreatedAt,
	}
	if t.DeletedAt != nil {
		out["deleted_at"] = t.DeletedAt
	}
	return out
}

func mustEnv(logger *zap.Logger, key string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		logger.Fatal("required env missing", zap.String("key", key))
	}
	return v
}

// buildAuditExporter wires the audit-log sink based on environment.
//
//	OBSERVE_X_AUDIT_LOG_BACKEND   = file | s3 | (unset → NopExporter)
//	OBSERVE_X_AUDIT_LOG_FILE_PATH = path for backend=file
//	OBSERVE_X_AUDIT_LOG_S3_BUCKET = bucket for backend=s3
//	OBSERVE_X_AUDIT_LOG_S3_PREFIX = key prefix (default "audit/")
//	OBSERVE_X_AUDIT_LOG_S3_REGION = AWS region
//	OBSERVE_X_AUDIT_LOG_S3_ENDPOINT = custom endpoint (MinIO/R2)
//	OBSERVE_X_AUDIT_LOG_S3_LOCK   = "" | GOVERNANCE | COMPLIANCE
//	OBSERVE_X_AUDIT_LOG_S3_RETAIN = duration (e.g. 8760h for 1y)
//
// The buffered wrapper isolates the synchronous request path from
// upload latency; flushes happen on a 60s ticker (or 1000 records).
func buildAuditExporter(ctx context.Context, logger *zap.Logger) (auditlog.Exporter, func(context.Context) error) {
	backend := os.Getenv("OBSERVE_X_AUDIT_LOG_BACKEND")
	switch backend {
	case "file":
		path := getEnv("OBSERVE_X_AUDIT_LOG_FILE_PATH", "/var/log/observex/audit.ndjson")
		fe, err := auditlog.NewFileExporter(path)
		if err != nil {
			logger.Warn("audit-log file exporter init failed; falling back to nop", zap.Error(err))
			return auditlog.NopExporter{}, func(context.Context) error { return nil }
		}
		buf := auditlog.NewBufferedExporter(fe, 4096)
		logger.Info("audit-log exporter active", zap.String("backend", "file"), zap.String("path", path))
		return buf, buf.Close
	case "s3":
		bucket := os.Getenv("OBSERVE_X_AUDIT_LOG_S3_BUCKET")
		if bucket == "" {
			logger.Warn("audit-log s3 bucket missing; falling back to nop")
			return auditlog.NopExporter{}, func(context.Context) error { return nil }
		}
		retain, _ := time.ParseDuration(os.Getenv("OBSERVE_X_AUDIT_LOG_S3_RETAIN"))
		s3e, err := auditlog.NewS3Exporter(ctx, auditlog.S3Options{
			Bucket:         bucket,
			Prefix:         os.Getenv("OBSERVE_X_AUDIT_LOG_S3_PREFIX"),
			Region:         os.Getenv("OBSERVE_X_AUDIT_LOG_S3_REGION"),
			Endpoint:       os.Getenv("OBSERVE_X_AUDIT_LOG_S3_ENDPOINT"),
			UseSSL:         os.Getenv("OBSERVE_X_AUDIT_LOG_S3_INSECURE") == "",
			ObjectLockMode: os.Getenv("OBSERVE_X_AUDIT_LOG_S3_LOCK"),
			RetainFor:      retain,
		})
		if err != nil {
			logger.Warn("audit-log s3 exporter init failed; falling back to nop", zap.Error(err))
			return auditlog.NopExporter{}, func(context.Context) error { return nil }
		}
		buf := auditlog.NewBufferedExporter(s3e, 4096)
		logger.Info("audit-log exporter active",
			zap.String("backend", "s3"),
			zap.String("bucket", bucket),
			zap.String("lock", os.Getenv("OBSERVE_X_AUDIT_LOG_S3_LOCK")))
		return buf, buf.Close
	default:
		return auditlog.NopExporter{}, func(context.Context) error { return nil }
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// splitCSV splits a comma-separated env value into trimmed,
// non-empty entries. Empty input ⇒ nil.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func ifZero(n, d int) int {
	if n == 0 {
		return d
	}
	return n
}

// ensure json import is used (Gin's BindJSON imports it transitively but
// some lint configurations want the explicit symbol).
var _ = json.Marshal
