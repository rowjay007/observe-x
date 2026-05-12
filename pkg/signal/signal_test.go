package signal

import (
	"testing"
	"time"
)

func TestSignalCreation(t *testing.T) {
	sig := Signal{
		TenantID:   "tenant-123",
		Type:       Metric,
		Payload:    []byte(`{"value": 42.5}`),
		Attributes: map[string]string{"service": "api"},
		ReceivedAt: time.Now(),
	}

	if sig.TenantID != "tenant-123" {
		t.Errorf("Expected tenant ID 'tenant-123', got '%s'", sig.TenantID)
	}

	if sig.Type != Metric {
		t.Errorf("Expected type Metric, got %s", sig.Type)
	}
}
