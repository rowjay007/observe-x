package integration

import (
	"context"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/wal"
)

func TestWALIntegration(t *testing.T) {
	tempDir := t.TempDir()

	w, err := wal.NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}
	defer w.Close()

	sig := signal.Signal{
		TenantID: "test-tenant",
		Type:     signal.Metric,
		Payload:  []byte(`{"value": 42.5}`),
	}

	err = w.Write(sig.Payload)
	if err != nil {
		t.Errorf("Failed to write to WAL: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			w.Write([]byte(`{"test": "data"}`))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("WAL write operations timed out")
	}
}
