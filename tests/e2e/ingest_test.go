package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/zeebo/blake3"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	gatewayURL    = "http://localhost:4318"
	gatewayIngest = gatewayURL + "/v1/ingest"
	gatewayHealth = gatewayURL + "/health"
)

func TestIngestGatewayE2E(t *testing.T) {
	const (
		tenantID = "e2e-tenant"
		secret   = "e2e-secret"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, err := startIngestGateway(ctx, secret)
	if err != nil {
		t.Fatalf("failed to start ingest gateway: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	apiKey, err := generateAPIKey(secret, tenantID)
	if err != nil {
		t.Fatalf("failed to generate api key: %v", err)
	}

	payload := map[string]interface{}{
		"tenant_id": tenantID,
		"type":      "METRIC",
		"payload":   []byte(`{"name": "http_requests_total", "value": 123}`),
		"attributes": map[string]string{
			"service": "api-gateway",
			"method":  "GET",
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, gatewayIngest, bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 202, got %d: %s", resp.StatusCode, string(body))
	}

	healthResp, err := client.Get(gatewayHealth)
	if err != nil {
		t.Fatalf("failed to check health: %v", err)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected health status 200, got %d", healthResp.StatusCode)
	}
}

func startIngestGateway(ctx context.Context, secret string) (*exec.Cmd, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(wd, "..", "..")
	cmd := exec.CommandContext(ctx, "go", "run", "./services/ingest-gateway/cmd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "OBSERVE_X_API_SECRET="+secret, "GIN_MODE=release")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			cmd.Wait()
			return nil, ctx.Err()
		default:
		}

		resp, err := client.Get(gatewayHealth)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return cmd, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = cmd.Process.Kill()
	cmd.Wait()
	return nil, fmt.Errorf("gateway did not become healthy")
}

func generateAPIKey(secret, tenantID string) (string, error) {
	h := blake3.New()
	if _, err := h.Write([]byte(secret)); err != nil {
		return "", err
	}
	if _, err := h.Write([]byte(":")); err != nil {
		return "", err
	}
	if _, err := h.Write([]byte(tenantID)); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s:%s", tenantID, hex.EncodeToString(h.Sum(nil))), nil
}
