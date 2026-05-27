package supervisor

import (
	"github.com/rowjay007/observe-x/pkg/signal"
	"testing"
	"time"
)

func TestSupervisorReusesActorsAndTracksHealth(t *testing.T) {
	sup := NewSupervisor()
	sup.Start()
	defer sup.Stop()

	first := sup.GetOrCreateActor("tenant-a")
	second := sup.GetOrCreateActor("tenant-a")
	if first != second {
		t.Fatalf("expected supervisor to reuse existing actor")
	}

	sup.RouteToTenant("tenant-a", signal.Signal{TenantID: "tenant-a", Type: signal.Log})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := sup.Stats()
		if stats.ActorCount == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stats := sup.Stats()
	if stats.ActorCount != 1 {
		t.Fatalf("expected 1 actor, got %d", stats.ActorCount)
	}
	if stats.TotalRouted == 0 {
		t.Fatalf("expected routed signals to be tracked")
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("failed to stop supervisor: %v", err)
	}
	if err := sup.Stop(); err != nil {
		t.Fatalf("expected supervisor stop to be idempotent, got %v", err)
	}
}
