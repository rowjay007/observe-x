package engine

import (
	"context"
	"testing"
	"time"

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

func TestProcessingEnginePipelineStages(t *testing.T) {
	engine, err := NewProcessingEngine(t.TempDir(), 0.1, 1000)
	if err != nil {
		t.Fatalf("failed to create processing engine: %v", err)
	}
	defer engine.Stop()

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}

	cases := []struct {
		sig        signal.Signal
		wantWrites int64
	}{
		{
			sig: signal.Signal{
				TenantID:   "tenant-a",
				Type:       signal.Log,
				Payload:    []byte(`{"message":"hello"}`),
				Attributes: map[string]string{"service_name": "api"},
			},
			wantWrites: 1,
		},
		{
			sig: signal.Signal{
				TenantID:   "tenant-b",
				Type:       signal.Log,
				Payload:    []byte(`{invalid-json}`),
				Attributes: map[string]string{"service_name": "api"},
			},
			wantWrites: 1,
		},
		{
			sig: signal.Signal{
				TenantID:   "",
				Type:       signal.Log,
				Payload:    []byte(`{"message":"hello"}`),
				Attributes: map[string]string{"service_name": "api"},
			},
			wantWrites: 1,
		},
	}

	for _, tc := range cases {
		if err := engine.ProcessSignal(ctx, tc.sig); err != nil {
			t.Fatalf("failed to process signal: %v", err)
		}
	}

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		_, _, walWrites := engine.Stats()
		if walWrites == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, _, walWrites := engine.Stats()
	if walWrites != 1 {
		t.Fatalf("expected 1 WAL write after pipeline processing, got %d", walWrites)
	}
}
