// ui-server is ObserveX's operator console — Phase C-4.
//
// It serves a single-page app (vanilla JS, embedded via go:embed)
// from `/`, and reverse-proxies `/api/tenant/*`, `/api/query/*`,
// `/api/alert/*` to the upstream tenant-api / query-engine /
// alert-manager services respectively.
//
// Auth: the operator authenticates via OIDC at the UI boundary
// (the same Phase C-3b validator as tenant-api). The validated
// bearer is then attached on the proxied request as
// `Authorization: Bearer <op-token>`, so each upstream service
// can run its own auth check. We deliberately do NOT re-issue a
// short-lived service-to-service token here — the operator IS the
// principal, and the audit trail should reflect that end-to-end.
//
// The UI is shipped as static assets embedded at compile time. No
// npm build step. No CDN dependency. `go build` produces a single
// binary that bundles index.html / app.js / app.css. This is a
// deliberate choice — see ADR-0017 for the rationale.
package main

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/oidc"
	"github.com/rowjay007/observe-x/pkg/selfobs"
)

//go:embed assets/*
var assetsFS embed.FS

type config struct {
	Addr           string
	TenantAPIURL   string
	QueryEngineURL string
	AlertManagerURL string

	OIDCIssuer      string
	OIDCAudience    string
	OIDCAdminGroups []string
}

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	cfg := loadConfig()

	tp, _ := selfobs.InitFromEnv(context.Background(), "ui-server", "1.0.0")
	if tp != nil {
		defer func() {
			c, cc := context.WithTimeout(context.Background(), 5*time.Second)
			defer cc()
			_ = tp.Shutdown(c)
		}()
	}

	// OIDC is OPTIONAL for the UI in single-tenant dev mode. We log
	// a loud warning when it isn't set; production deployments must
	// enable it (the Helm chart's NOTES.txt repeats this).
	var validator *oidc.Validator
	if cfg.OIDCIssuer != "" {
		v, err := oidc.NewValidator(context.Background(), oidc.Config{
			Issuer:      cfg.OIDCIssuer,
			Audience:    cfg.OIDCAudience,
			AdminGroups: cfg.OIDCAdminGroups,
		})
		if err != nil {
			logger.Fatal("oidc init", zap.Error(err))
		}
		defer v.Close()
		validator = v
		logger.Info("oidc enabled",
			zap.String("issuer", cfg.OIDCIssuer),
			zap.String("audience", cfg.OIDCAudience),
			zap.Strings("admin_groups", cfg.OIDCAdminGroups))
	} else {
		logger.Warn("OIDC disabled — single-tenant dev mode")
	}

	mux := http.NewServeMux()

	// Healthz / readyz / metrics — unauthenticated, scraped by ops.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.Handle("/metrics", observability.MetricsHandler())

	// Static assets — serve the embedded SPA from /.
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		logger.Fatal("assets sub", zap.Error(err))
	}
	mux.Handle("/", spaHandler(sub))

	// Upstream proxies — each is mounted behind the OIDC middleware
	// when OIDC is configured. The proxy forwards the operator's
	// bearer to the upstream service.
	mux.Handle("/api/tenant/", proxyHandler(cfg.TenantAPIURL, "/api/tenant", validator, logger))
	mux.Handle("/api/query/", proxyHandler(cfg.QueryEngineURL, "/api/query", validator, logger))
	mux.Handle("/api/alert/", proxyHandler(cfg.AlertManagerURL, "/api/alert", validator, logger))

	// Surface the resolved config to the UI so the SPA knows which
	// issuer to authenticate against for the in-browser OIDC flow.
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{` +
			`"oidc_issuer":"` + cfg.OIDCIssuer + `",` +
			`"oidc_audience":"` + cfg.OIDCAudience + `",` +
			`"version":"1.0.0"` +
			`}`))
	})

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // queries can be slow
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("ui-server listening", zap.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// spaHandler serves embedded static files. If the requested path
// doesn't match a file, it falls back to index.html so client-side
// routing keeps working on deep links.
func spaHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject directory traversal attempts.
		if strings.Contains(r.URL.Path, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		// File exists? serve it; else fall back to index.html.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			r.URL.Path = "/"
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// CSP allows inline styles for the bundled CSS but no
		// external scripts; the SPA is self-contained.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		fileServer.ServeHTTP(w, r)
	})
}

// proxyHandler wraps a reverse-proxy in optional OIDC validation.
func proxyHandler(target, stripPrefix string, validator *oidc.Validator, logger *zap.Logger) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		logger.Fatal("proxy target", zap.String("url", target), zap.Error(err))
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Director = directorFor(u, stripPrefix)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		logger.Warn("upstream", zap.String("target", u.String()), zap.Error(err))
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	if validator == nil {
		return rp
	}
	return validator.Middleware(rp)
}

// directorFor returns a Director that rewrites the request URL to
// hit the upstream and strips the UI-side prefix. The operator's
// Authorization header is passed through unmodified.
func directorFor(u *url.URL, stripPrefix string) func(*http.Request) {
	return func(r *http.Request) {
		r.URL.Scheme = u.Scheme
		r.URL.Host = u.Host
		r.URL.Path = strings.TrimPrefix(r.URL.Path, stripPrefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.Host = u.Host
		// Preserve the original client IP for upstream audit logs.
		if prior, ok := r.Header["X-Forwarded-For"]; ok {
			r.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+remoteIP(r))
		} else {
			r.Header.Set("X-Forwarded-For", remoteIP(r))
		}
		r.Header.Set("X-Forwarded-Proto", "https")
	}
}

func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

func loadConfig() config {
	return config{
		Addr:           getEnv("OBSERVE_X_UI_ADDR", ":8080"),
		TenantAPIURL:   getEnv("OBSERVE_X_TENANT_API_URL", "http://tenant-api:8081"),
		QueryEngineURL: getEnv("OBSERVE_X_QUERY_ENGINE_URL", "http://query-engine:8082"),
		AlertManagerURL: getEnv("OBSERVE_X_ALERT_MANAGER_URL", "http://alert-manager:8083"),

		OIDCIssuer:      os.Getenv("OBSERVE_X_OIDC_ISSUER"),
		OIDCAudience:    getEnv("OBSERVE_X_OIDC_AUDIENCE", "observex"),
		OIDCAdminGroups: splitCSV(os.Getenv("OBSERVE_X_OIDC_ADMIN_GROUPS")),
	}
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

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
