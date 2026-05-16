package main

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/auth"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	apiSecret, ok := os.LookupEnv("OBSERVE_X_API_SECRET")
	if !ok || apiSecret == "" {
		log.Fatal("OBSERVE_X_API_SECRET environment variable is required")
	}

	processingEngine, err := engine.NewProcessingEngine("/tmp/observex/wal", 0.1, 1000)
	if err != nil {
		log.Fatalf("Failed to create processing engine: %v", err)
	}
	defer processingEngine.Stop()

	ctx := context.Background()
	if err := processingEngine.Start(ctx); err != nil {
		log.Fatalf("Failed to start processing engine: %v", err)
	}

	authMiddleware := auth.NewAuthMiddleware(auth.NewStatelessKeyValidator(apiSecret))
	r := buildRouter(authMiddleware, processingEngine, ctx)

	server := &http.Server{
		Addr:    ":4318",
		Handler: r,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("failed to start server: %v", err)
	}
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
			Type     signal.Type       `json:"type"`
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

		sig := signal.Signal{
			TenantID:   tenantID,
			Type:       req.Type,
			Payload:    req.Payload,
			Attributes: req.Attrs,
			ReceivedAt: time.Now(),
		}

		if err := processingEngine.ProcessSignal(ctx, sig); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Processing failed"})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{
			"status":    "accepted",
			"timestamp": time.Now().Unix(),
		})
	}
}
