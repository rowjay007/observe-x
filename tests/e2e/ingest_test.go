package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestIngestGatewayE2E(t *testing.T) {
	baseURL := "http://localhost:4318"
	
	payload := map[string]interface{}{
		"tenant_id": "e2e-tenant",
		"type":      "METRIC",
		"payload":   []byte(`{"name": "http_requests_total", "value": 123}`),
		"attributes": map[string]string{
			"service": "api-gateway",
			"method":  "GET",
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(baseURL+"/v1/ingest", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", resp.StatusCode)
	}

	healthResp, err := client.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("Failed to check health: %v", err)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("Expected health status 200, got %d", healthResp.StatusCode)
	}
}
