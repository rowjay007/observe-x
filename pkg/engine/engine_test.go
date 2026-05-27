package engine

import (
	"context"
	"testing"

	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestProcessSingleSignalPersistsToWAL(t *testing.T) {
	engine, err := NewProcessingEngine(t.TempDir(), 0.1, 1000)
	if err != nil {
		t.Fatalf("failed to create processing engine: %v", err)
	}
	defer engine.Stop()

	sig := signal.Signal{
		TenantID:   "tenant-a",
		Type:       signal.Log,
		Payload:    []byte(`{"message":"hello"}`),
		Attributes: map[string]string{"service_name": "api"},
	}

	engine.processSingleSignal(context.Background(), sig)

	if _, _, walWrites := engine.Stats(); walWrites != 1 {
		t.Fatalf("expected 1 WAL write, got %d", walWrites)
	}
}
