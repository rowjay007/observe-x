package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	r := gin.New()
	r.Use(gin.Recovery())

	r.POST("/v1/ingest", func(c *gin.Context) {
		c.JSON(http.StatusAccepted, gin.H{
			"status":    "accepted",
			"timestamp": time.Now().Unix(),
		})
	})

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if err := r.Run(":4318"); err != nil {
		logger.Fatal("failed to start server", zap.Error(err))
	}
}
