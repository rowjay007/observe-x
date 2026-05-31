// alert-manager turns CEP events and explicit SLO-burn signals into
// notifications. It owns:
//
//   - Postgres-backed alert state (firing/resolved transitions, dedup,
//     append-only audit history, operator silences).
//   - A notifier Dispatcher fanning out to Slack/PagerDuty/Webhook.
//   - An in-process sloburn.Evaluator that can score SLO burn rates.
//
// HTTP surface (authenticated via the same pkg/auth.PostgresKeyStore
// as the rest of ObserveX):
//
//   POST /v1/events             CEP/sloburn pushes an event
//   POST /v1/observations       per-event good/bad for an SLO
//   GET  /v1/alerts             list active alerts for tenant
//   POST /v1/silences           operator-applied label-matcher silence
//   GET  /health, /ready, /metrics
//
// Notifier configuration is fully env-driven:
//
//   OBSERVE_X_SLACK_WEBHOOK_URL
//   OBSERVE_X_PAGERDUTY_INTEGRATION_KEY
//   OBSERVE_X_WEBHOOK_URL
//
// Missing config ⇒ that notifier isn't registered (alert still
// persists; just doesn't dispatch externally). At least one notifier
// is required at startup, else the service exits with a fatal error.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/auditlog"
	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/notifier"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/selfobs"
	"github.com/rowjay007/observe-x/pkg/sloburn"
	"github.com/rowjay007/observe-x/services/alert-manager/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	tp, _ := selfobs.InitFromEnv(context.Background(), "alert-manager", "1.0.0")
	if tp != nil {
		defer func() {
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			defer cc()
			_ = tp.Shutdown(c)
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn := os.Getenv("OBSERVE_X_POSTGRES_URL")
	if dsn == "" {
		logger.Fatal("OBSERVE_X_POSTGRES_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Fatal("postgres pool", zap.Error(err))
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		logger.Fatal("migrate", zap.Error(err))
	}
	st := store.New(pool)

	dispatcher, err := buildDispatcher(logger)
	if err != nil {
		logger.Fatal("notifier", zap.Error(err))
	}

	evaluator := sloburn.New()

	keyStore, keyStoreClose, err := initKeyStore(ctx, logger)
	if err != nil {
		logger.Fatal("auth init", zap.Error(err))
	}
	defer keyStoreClose()
	authMW := auth.NewAuthMiddleware(keyStore)

	auditExp, auditCloser := buildAuditExporter(ctx, logger)
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := auditCloser(shutdownCtx); err != nil {
			logger.Warn("audit-log close", zap.Error(err))
		}
	}()

	router := buildRouter(authMW, st, dispatcher, evaluator, auditExp, logger)

	srv := &http.Server{
		Addr:         getEnv("OBSERVE_X_ALERT_ADDR", ":7700"),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("alert-manager listening",
			zap.String("addr", srv.Addr),
			zap.Strings("notifiers", dispatcher.Names()))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("alert-manager stopped")
}

// ─── Router ──────────────────────────────────────────────────────────────

func buildRouter(authMW *auth.AuthMiddleware, st *store.Store, dispatcher *notifier.Dispatcher,
	evaluator *sloburn.Evaluator, auditExp auditlog.Exporter, logger *zap.Logger) http.Handler {

	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ready"}) })
	r.GET("/metrics", gin.WrapH(observability.MetricsHandler()))

	authed := r.Group("/")
	authed.Use(ginAuth(authMW))
	// Scope policy (see ADR-0011):
	//   /v1/events         alert.write — anyone pushing an alert is
	//                      effectively writing to the alerting plane
	//   /v1/observations   alert.write — same reasoning (drives SLO state)
	//   /v1/alerts         alert.read  — read-only listing
	//   /v1/silences       alert.write — operators-suppress requires write
	//   /v1/slos           tenant.admin — SLO definition is a control-plane mutation
	authed.POST("/v1/events", auth.GinRequireScope(auth.ScopeAlertWrite), eventsHandler(st, dispatcher, logger))
	authed.POST("/v1/observations", auth.GinRequireScope(auth.ScopeAlertWrite), observationsHandler(evaluator, st, dispatcher, logger))
	authed.GET("/v1/alerts", auth.GinRequireScope(auth.ScopeAlertRead), listAlertsHandler(st))
	authed.POST("/v1/silences", auth.GinRequireScope(auth.ScopeAlertWrite), createSilenceHandler(st, auditExp, logger))
	authed.POST("/v1/slos", auth.GinRequireScope(auth.ScopeTenantAdmin), registerSLOHandler(evaluator, auditExp, logger))

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

// ─── Handlers ────────────────────────────────────────────────────────────

type incomingEvent struct {
	RuleID      string            `json:"rule_id"`
	Severity    string            `json:"severity"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Resolved    bool              `json:"resolved"`
	OccurredAt  string            `json:"occurred_at,omitempty"`
}

func eventsHandler(st *store.Store, dispatcher *notifier.Dispatcher, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		var req incomingEvent
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.RuleID == "" || req.Severity == "" || req.Title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rule_id, severity, title are required"})
			return
		}
		fp := store.Fingerprint(tenantID, req.RuleID, req.Labels)
		now := time.Now().UTC()
		occurredAt := now
		if req.OccurredAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, req.OccurredAt); err == nil {
				occurredAt = t
			}
		}

		if req.Resolved {
			if err := st.Resolve(c.Request.Context(), fp, now); err != nil {
				logger.Warn("resolve", zap.Error(err))
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			// Best-effort resolved notification (no dedup window check).
			go dispatchResolved(context.Background(), dispatcher, tenantID, fp, req, now, logger)
			c.JSON(http.StatusAccepted, gin.H{"status": "resolved", "fingerprint": fp})
			return
		}

		silenced, err := st.IsSilenced(c.Request.Context(), tenantID, req.Labels)
		if err != nil {
			logger.Warn("silence check", zap.Error(err))
		}

		newTransition, err := st.UpsertFiring(c.Request.Context(), store.Alert{
			Fingerprint: fp, TenantID: tenantID, RuleID: req.RuleID,
			Severity: req.Severity, Title: req.Title, Description: req.Description,
			Labels: req.Labels, Annotations: req.Annotations,
			StartsAt: occurredAt, LastSeenAt: now,
		})
		if err != nil {
			logger.Warn("upsert firing", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		dispatched := false
		if newTransition && !silenced {
			go dispatchFiring(context.Background(), dispatcher, st, tenantID, fp, req, now, logger)
			dispatched = true
		}

		c.JSON(http.StatusAccepted, gin.H{
			"status":       "received",
			"fingerprint":  fp,
			"new_transition": newTransition,
			"silenced":     silenced,
			"dispatched":   dispatched,
		})
	}
}

func dispatchFiring(ctx context.Context, dispatcher *notifier.Dispatcher, st *store.Store,
	tenantID, fp string, req incomingEvent, at time.Time, logger *zap.Logger) {
	n := notifier.Notification{
		Fingerprint: fp, TenantID: tenantID,
		Severity:    notifier.Severity(req.Severity),
		Title:       req.Title, Description: req.Description,
		Labels:      req.Labels, Annotations: req.Annotations,
		Source:      "alert-manager", StartsAt: at,
	}
	if err := dispatcher.Dispatch(ctx, n); err != nil {
		logger.Warn("dispatch", zap.String("fp", fp), zap.Error(err))
		return
	}
	if err := st.MarkNotified(ctx, fp, time.Now()); err != nil {
		logger.Warn("mark notified", zap.Error(err))
	}
}

func dispatchResolved(ctx context.Context, dispatcher *notifier.Dispatcher,
	tenantID, fp string, req incomingEvent, at time.Time, logger *zap.Logger) {
	n := notifier.Notification{
		Fingerprint: fp, TenantID: tenantID,
		Severity:    notifier.Severity(req.Severity),
		Title:       req.Title, Description: req.Description,
		Labels:      req.Labels, Annotations: req.Annotations,
		Source:      "alert-manager", StartsAt: at, EndsAt: at,
	}
	if err := dispatcher.Dispatch(ctx, n); err != nil {
		logger.Warn("dispatch resolved", zap.String("fp", fp), zap.Error(err))
	}
}

// ─── Observations: drive sloburn directly ────────────────────────────────

type observationReq struct {
	SLO  string `json:"slo"`
	Good bool   `json:"good"`
}

func observationsHandler(eval *sloburn.Evaluator, st *store.Store,
	dispatcher *notifier.Dispatcher, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		var req observationReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		sloName := scopedSLOName(tenantID, req.SLO)
		if err := eval.Observe(sloName, req.Good, time.Now().UTC()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		d, err := eval.Evaluate(sloName, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if d.Severity != sloburn.SevOK {
			labels := map[string]string{"slo": req.SLO, "severity": string(d.Severity)}
			fp := store.Fingerprint(tenantID, "slo:"+req.SLO, labels)
			newTransition, err := st.UpsertFiring(c.Request.Context(), store.Alert{
				Fingerprint: fp, TenantID: tenantID, RuleID: "slo:" + req.SLO,
				Severity:    string(d.Severity),
				Title:       "SLO burning: " + req.SLO,
				Description: formatBurnDescription(d),
				Labels:      labels,
				StartsAt:    time.Now().UTC(),
				LastSeenAt:  time.Now().UTC(),
			})
			if err == nil && newTransition {
				go dispatchFiring(context.Background(), dispatcher, st, tenantID, fp, incomingEvent{
					RuleID: "slo:" + req.SLO, Severity: string(d.Severity),
					Title: "SLO burning: " + req.SLO, Description: formatBurnDescription(d),
					Labels: labels,
				}, time.Now().UTC(), logger)
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"severity":   d.Severity,
			"burn_long":  d.BurnLong,
			"burn_short": d.BurnShort,
		})
	}
}

func registerSLOHandler(eval *sloburn.Evaluator, auditExp auditlog.Exporter, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		var req struct {
			Name   string  `json:"name"`
			Target float64 `json:"target"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		slo := sloburn.SLO{
			Name:        scopedSLOName(tenantID, req.Name),
			Target:      req.Target,
			WindowPairs: sloburn.DefaultWindowPairs(),
		}
		if err := eval.Register(slo); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		emitAudit(c, auditExp, logger, tenantID, "slo.register", map[string]any{
			"name":   req.Name,
			"target": req.Target,
		})
		c.JSON(http.StatusCreated, gin.H{"slo": req.Name})
	}
}

func listAlertsHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		alerts, err := st.ListAlerts(c.Request.Context(), tenantID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"alerts": alerts})
	}
}

func createSilenceHandler(st *store.Store, auditExp auditlog.Exporter, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}
		var req struct {
			Matcher    map[string]string `json:"matcher"`
			Reason     string            `json:"reason"`
			DurationS  int               `json:"duration_seconds"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.DurationS <= 0 || req.DurationS > 7*24*3600 {
			req.DurationS = 3600
		}
		expiresAt := time.Now().Add(time.Duration(req.DurationS) * time.Second)
		id, err := st.CreateSilence(c.Request.Context(), store.Silence{
			TenantID: tenantID, Matcher: req.Matcher, Reason: req.Reason,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		emitAudit(c, auditExp, logger, tenantID, "silence.create", map[string]any{
			"id":         id,
			"reason":     req.Reason,
			"matcher":    req.Matcher,
			"expires_at": expiresAt,
		})
		c.JSON(http.StatusCreated, gin.H{"id": id})
	}
}

// emitAudit pushes a record through the configured exporter, never
// blocking the request path on a hard failure.
func emitAudit(c *gin.Context, exp auditlog.Exporter, logger *zap.Logger, tenantID, action string, details map[string]any) {
	if exp == nil {
		return
	}
	srcIP := c.ClientIP()
	if err := exp.Append(c.Request.Context(), auditlog.Record{
		TenantID:   tenantID,
		Actor:      "tenant", // gated by tenant API key, not operator
		Action:     action,
		Details:    details,
		SourceIP:   srcIP,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		logger.Warn("audit-log export failed",
			zap.Error(err), zap.String("action", action))
	}
}

// buildAuditExporter mirrors the tenant-api helper — same env knobs
// so a single deployment can configure both services identically.
func buildAuditExporter(ctx context.Context, logger *zap.Logger) (auditlog.Exporter, func(context.Context) error) {
	backend := os.Getenv("OBSERVE_X_AUDIT_LOG_BACKEND")
	switch backend {
	case "file":
		path := getEnv("OBSERVE_X_AUDIT_LOG_FILE_PATH", "/var/log/observex/audit-alert-manager.ndjson")
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

// ─── helpers ─────────────────────────────────────────────────────────────

func buildDispatcher(logger *zap.Logger) (*notifier.Dispatcher, error) {
	var notifiers []notifier.Notifier
	if u := os.Getenv("OBSERVE_X_SLACK_WEBHOOK_URL"); u != "" {
		notifiers = append(notifiers, notifier.NewSlackNotifier(u))
	}
	if k := os.Getenv("OBSERVE_X_PAGERDUTY_INTEGRATION_KEY"); k != "" {
		notifiers = append(notifiers, notifier.NewPagerDutyNotifier(k))
	}
	if u := os.Getenv("OBSERVE_X_WEBHOOK_URL"); u != "" {
		headers := map[string]string{}
		if t := os.Getenv("OBSERVE_X_WEBHOOK_TOKEN"); t != "" {
			headers["Authorization"] = "Bearer " + t
		}
		notifiers = append(notifiers, notifier.NewWebhookNotifier(u, headers))
	}
	if len(notifiers) == 0 {
		return nil, errors.New("at least one notifier must be configured " +
			"(OBSERVE_X_SLACK_WEBHOOK_URL / OBSERVE_X_PAGERDUTY_INTEGRATION_KEY / OBSERVE_X_WEBHOOK_URL)")
	}
	logger.Info("dispatcher", zap.Strings("notifiers", notifierNames(notifiers)))
	return notifier.NewDispatcher(notifiers), nil
}

func notifierNames(ns []notifier.Notifier) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name()
	}
	return out
}

func formatBurnDescription(d sloburn.Decision) string {
	return strings.Join([]string{
		"long-window burn rate: ", strconv.FormatFloat(d.BurnLong, 'f', 2, 64), "x; ",
		"short-window: ", strconv.FormatFloat(d.BurnShort, 'f', 2, 64), "x; ",
		"error budget: ", strconv.FormatFloat(d.ErrorBudget*100, 'f', 3, 64), "%",
	}, "")
}

func scopedSLOName(tenantID, name string) string { return tenantID + ":" + name }

func initKeyStore(ctx context.Context, logger *zap.Logger) (auth.KeyStore, func(), error) {
	if dsn := os.Getenv("OBSERVE_X_POSTGRES_URL"); dsn != "" {
		ks, err := auth.NewPostgresKeyStore(ctx, dsn, auth.PostgresOptions{})
		if err != nil {
			return nil, func() {}, err
		}
		logger.Info("auth: PostgresKeyStore active")
		return ks, func() { _ = ks.Close() }, nil
	}
	apiSecret, ok := os.LookupEnv("OBSERVE_X_API_SECRET")
	if !ok || apiSecret == "" {
		return nil, func() {}, errors.New("OBSERVE_X_POSTGRES_URL or OBSERVE_X_API_SECRET required")
	}
	logger.Warn("auth: StatelessKeyValidator active (DEV ONLY)")
	return auth.NewStatelessKeyValidator(apiSecret), func() {}, nil
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
