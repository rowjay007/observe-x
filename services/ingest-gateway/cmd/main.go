package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/supervisor"
	"github.com/rowjay007/observe-x/pkg/wal"
)

func main() {
	walInstance, err := wal.NewWAL("/tmp/observex/wal")
	if err != nil {
		log.Fatalf("Failed to initialize WAL: %v", err)
	}
	defer walInstance.Close()

	supervisorInstance := supervisor.NewSupervisor()
	supervisorInstance.Start()
	defer supervisorInstance.Stop()

	r := gin.New()
	r.Use(gin.Recovery())

	r.POST("/v1/ingest", func(c *gin.Context) {
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

		sig := signal.Signal{
			TenantID:   req.TenantID,
			Type:       req.Type,
			Payload:    req.Payload,
			Attributes: req.Attrs,
			ReceivedAt: time.Now(),
		}

		supervisorInstance.RouteToTenant(req.TenantID, sig)

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
