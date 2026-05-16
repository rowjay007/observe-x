package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/services/ingest-gateway/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildRouterRequiresAuth(t *testing.T) {
	processingEngine, err := engine.NewProcessingEngine(t.TempDir()+"/wal", 0.1, 1000)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer processingEngine.Stop()

	ctx := context.Background()
	if err := processingEngine.Start(ctx); err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}

	secret := "test-secret"
	authMiddleware := auth.NewAuthMiddleware(auth.NewStatelessKeyValidator(secret))
	router := buildRouter(authMiddleware, processingEngine, ctx)

	payload := map[string]interface{}{
		"tenant_id":  "tenant-123",
		"type":       "log",
		"payload":    "Zm9v",
		"attributes": map[string]string{"source": "test"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without auth, got %d", resp.Code)
	}

	apiKey := auth.GenerateAPIKey(secret, "tenant-123")
	req = httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp = httptest.NewRecorder()

	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected accepted with valid auth, got %d", resp.Code)
	}
}
