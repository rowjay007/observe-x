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
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/services/tenant-api/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	dsn := mustEnv(logger, "OBSERVE_X_POSTGRES_URL")
	adminToken := mustEnv(logger, "OBSERVE_X_TENANT_API_ADMIN_TOKEN")

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

	srv := &server{
		logger:     logger,
		adminToken: adminToken,
		repo:       repo,
		keyStore:   keyStore,
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
	repo       *store.Store
	keyStore   *auth.PostgresKeyStore
	once       sync.Once
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
	}
	return r
}

// ─── admin auth ──────────────────────────────────────────────────────────

func (s *server) requireAdmin() gin.HandlerFunc {
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
}

func (s *server) issueAPIKey(c *gin.Context) {
	tenantID := c.Param("id")
	if _, err := s.repo.GetTenant(c.Request.Context(), tenantID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	var req issueKeyReq
	_ = c.BindJSON(&req) // body is optional

	issued, err := s.keyStore.IssueKey(c.Request.Context(), tenantID, req.ExpiresAt)
	if err != nil {
		s.logger.Error("issue key", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue failed"})
		return
	}
	s.audit(c, &tenantID, "admin", "api_key.issue", map[string]any{
		"kid":    issued.KID,
		"prefix": issued.Prefix,
	})
	c.JSON(http.StatusCreated, map[string]any{
		"kid":        issued.KID,
		"raw_key":    issued.Raw, // show ONCE
		"prefix":     issued.Prefix,
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

// ─── helpers ─────────────────────────────────────────────────────────────

func (s *server) audit(c *gin.Context, tenantID *string, actor, action string, details map[string]any) {
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

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
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
