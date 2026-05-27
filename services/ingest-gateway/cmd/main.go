package main

import (
	"context"
	"crypto/tls"
	"github.com/gin-gonic/gin"
	"github.com/rowjay007/observe-x/pkg/engine"
	pkgsignal "github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/auth"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/otlp"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/receiver"
	"go.uber.org/zap"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Sync()

	apiSecret, ok := os.LookupEnv("OBSERVE_X_API_SECRET")
	if !ok || apiSecret == "" {
		log.Fatal("OBSERVE_X_API_SECRET environment variable is required")
	}

	walPath := getEnv("OBSERVE_X_WAL_PATH", "/tmp/observex/wal")

	processingEngine, err := engine.NewProcessingEngine(walPath, 0.1, 1000)
	if err != nil {
		log.Fatalf("Failed to create processing engine: %v", err)
	}
	defer processingEngine.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := processingEngine.Start(ctx); err != nil {
		log.Fatalf("Failed to start processing engine: %v", err)
	}

	keyStore := auth.NewStatelessKeyValidator(apiSecret)
	authMiddleware := auth.NewAuthMiddleware(keyStore)

	otlpSignalChan := make(chan pkgsignal.Signal, 4096)
	go func() {
		for {
			select {
			case sig, ok := <-otlpSignalChan:
				if !ok {
					return
				}
				_ = processingEngine.ProcessSignal(ctx, sig)
			case <-ctx.Done():
				return
			}
		}
	}()

	serverTLSConfig, err := loadServerTLSConfig()
	if err != nil {
		log.Fatalf("failed to load TLS config: %v", err)
	}

	// ── HTTP Server on :4318 ───────────────────────────────────────────
	r := buildRouter(authMiddleware, processingEngine, ctx)
	r.POST("/v1/otlp/traces", func(c *gin.Context) {
		tenantID := c.GetHeader("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}

		payload, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(payload) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "empty otlp payload"})
			return
		}

		receiver := otlp.NewTraceReceiver(otlpSignalChan, tenantID)
		if err := receiver.Accept(c.Request.Context(), payload); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
	})

	httpServer := &http.Server{
		Addr:         getEnv("OBSERVE_X_HTTP_ADDR", ":4318"),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── gRPC Server on :4317 ───────────────────────────────────────────
	grpcAddr := getEnv("OBSERVE_X_GRPC_ADDR", ":4317")
	grpcReceiver := receiver.NewGRPCReceiver(grpcAddr, processingEngine, keyStore, logger, serverTLSConfig)

	// ── StatsD UDP Server on :8125 ─────────────────────────────────────
	statsdAddr := getEnv("OBSERVE_X_STATSD_ADDR", ":8125")
	defaultTenant := getEnv("OBSERVE_X_DEFAULT_TENANT", "default")
	statsdReceiver := receiver.NewStatsDReceiver(statsdAddr, processingEngine, defaultTenant, logger)

	// ── Start all servers concurrently ──────────────────────────────────
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("HTTP server starting", zap.String("addr", httpServer.Addr))
		if err := serveHTTP(httpServer, serverTLSConfig, logger); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := grpcReceiver.Start(ctx); err != nil {
			logger.Error("gRPC receiver failed", zap.Error(err))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := statsdReceiver.Start(ctx); err != nil {
			logger.Error("StatsD receiver failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down servers...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server forced shutdown", zap.Error(err))
	}

	grpcReceiver.Stop()
	statsdReceiver.Stop()

	wg.Wait()
	logger.Info("all servers stopped")
}

func buildRouter(authMiddleware *auth.AuthMiddleware, processingEngine *engine.ProcessingEngine, ctx context.Context) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	authorized := r.Group("/")
	authorized.Use(func(c *gin.Context) {
		authMiddleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)

		if c.Writer.Written() {
			c.Abort()
			return
		}
	})

	authorized.POST("/v1/ingest", ingestHandler(processingEngine, ctx))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return r
}

func ingestHandler(processingEngine *engine.ProcessingEngine, ctx context.Context) gin.HandlerFunc {
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "tenant id mismatch between auth and request"})
			return
		}

		sig := pkgsignal.Signal{
			TenantID:   tenantID,
			Type:       req.Type,
			Payload:    req.Payload,
			Attributes: req.Attrs,
			ReceivedAt: time.Now(),
		}

		if err := processingEngine.ProcessSignal(ctx, sig); err != nil {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "system overloaded, try again later"})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{
			"status":    "accepted",
			"timestamp": time.Now().Unix(),
		})
	}
}

func serveHTTP(server *http.Server, tlsConfig *tls.Config, logger *zap.Logger) error {
	if tlsConfig == nil {
		logger.Info("HTTP server starting", zap.String("addr", server.Addr))
		return server.ListenAndServe()
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}

	return server.Serve(tls.NewListener(listener, tlsConfig))
}

func loadServerTLSConfig() (*tls.Config, error) {
	certFile := getEnv("OBSERVE_X_TLS_CERT_FILE", "")
	keyFile := getEnv("OBSERVE_X_TLS_KEY_FILE", "")
	caFile := getEnv("OBSERVE_X_TLS_CA_FILE", "")

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
