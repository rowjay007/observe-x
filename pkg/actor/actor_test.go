package actor

import (
	"context"
	"github.com/rowjay007/observe-x/pkg/cep"
	"github.com/rowjay007/observe-x/pkg/signal"
	"testing"
	"time"
)

func TestTenantActorProcessesSignalsAndTracksStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Lower the threshold so a 6-error synthetic burst trips the
	// rule under the Phase B-4 errors/sec semantics.
	actor := NewTenantActorWithOptions("test-tenant", 10, Options{
		ErrorRateThresholdEPS: 0.001, // ~1 error per 1000s; 6 trivially exceeds
		Window:                5 * time.Minute,
	})
	if err := actor.Start(ctx); err != nil {
		t.Fatalf("failed to start actor: %v", err)
	}

	for i := 0; i < 6; i++ {
		actor.Mailbox() <- signal.Signal{
			TenantID: "test-tenant",
			Type:     signal.Log,
			Attributes: map[string]string{
				"severity":     "ERROR",
				"service_name": "api",
			},
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := actor.Stats()
		if stats.Processed == 6 && stats.LastEventType == cep.HighErrorRate {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stats := actor.Stats()
	if stats.Processed != 6 {
		t.Fatalf("expected 6 processed signals, got %d", stats.Processed)
	}
	if stats.LastEventType != cep.HighErrorRate {
		t.Fatalf("expected high error rate event, got %q", stats.LastEventType)
	}

	if err := actor.Stop(); err != nil {
		t.Fatalf("failed to stop actor: %v", err)
	}
	if err := actor.Stop(); err != nil {
		t.Fatalf("expected stop to be idempotent, got %v", err)
	}
}
