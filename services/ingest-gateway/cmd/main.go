package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/engine"
)

func main() {
	processingEngine, err := engine.NewProcessingEngine("/tmp/observex/wal", 0.1, 1000)
	if err != nil {
		log.Fatalf("Failed to create processing engine: %v", err)
	}
	defer processingEngine.Stop()

	ctx := context.Background()
	if err := processingEngine.Start(ctx); err != nil {
		log.Fatalf("Failed to start processing engine: %v", err)
	}

	r := gin.New()
	r.Use(gin.Recovery())

	r.POST("/v1/ingest", func(c *gin.Context) {
		var req struct {
			TenantID string            `json:"tenant_id"`
			Type     signal.Type        `json:"type"`
			Payload  []byte           `json:"payload"`
			Attrs    map[string]string `json:"attributes"`
		}

		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		sig := signal.Signal{
			TenantID:   req.TenantID,
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
	})

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if err := r.Run(":4318"); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
