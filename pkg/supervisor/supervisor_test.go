package supervisor

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
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

func TestSupervisorRestartsCrashedActor(t *testing.T) {
	sup := NewSupervisorWithOptions(Options{
		MaxRestarts:    10,
		RestartWindow:  time.Minute,
		BackoffMin:     5 * time.Millisecond,
		BackoffMax:     20 * time.Millisecond,
		HealthInterval: 25 * time.Millisecond,
	})
	sup.Start()
	defer sup.Stop()

	a := sup.GetOrCreateActor("tenant-r")
	if a == nil {
		t.Fatal("missing actor")
	}
	// Simulate crash.
	_ = a.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.Stats().TotalRestarts >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.Stats().TotalRestarts; got < 1 {
		t.Fatalf("want >=1 restart, got %d", got)
	}

	// The post-restart actor should be a fresh instance and running.
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		now := sup.GetOrCreateActor("tenant-r")
		if now != nil && now.IsRunning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := sup.GetOrCreateActor("tenant-r"); got == nil || !got.IsRunning() {
		t.Fatalf("post-restart actor not running")
	}
}

func TestSupervisorQuarantinesAfterRestartFlood(t *testing.T) {
	var quarantines atomic.Int32
	sup := NewSupervisorWithOptions(Options{
		MaxRestarts:    2,
		RestartWindow:  time.Second,
		BackoffMin:     1 * time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		HealthInterval: 10 * time.Millisecond,
	})
	sup.onCrash = func(string, any) { quarantines.Add(1) }
	sup.Start()
	defer sup.Stop()

	// Force repeated crashes to trip the quarantine.
	for i := 0; i < 6; i++ {
		a := sup.GetOrCreateActor("flaky")
		if a == nil {
			break // already quarantined
		}
		_ = a.Stop()
		time.Sleep(40 * time.Millisecond)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if quarantines.Load() >= 1 || sup.Stats().Quarantined >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if sup.Stats().Quarantined < 1 {
		t.Fatalf("expected quarantine, stats=%+v", sup.Stats())
	}
	// Routing to a quarantined tenant should drop, not panic.
	sup.RouteToTenant("flaky", signal.Signal{TenantID: "flaky", Type: signal.Log})
	if sup.Stats().TotalDropped < 1 {
		t.Fatalf("expected drop counter to advance")
	}

	// Operator releases quarantine — next route should re-create.
	sup.ReleaseQuarantine("flaky")
	a := sup.GetOrCreateActor("flaky")
	if a == nil || !a.IsRunning() {
		t.Fatalf("release-quarantine did not allow fresh actor")
	}
}

func TestBackoffMonotonicWithJitter(t *testing.T) {
	curr := 100 * time.Millisecond
	max := time.Second
	for i := 0; i < 20; i++ {
		next := nextBackoff(curr, max)
		if next > max {
			t.Fatalf("next %s exceeded max %s", next, max)
		}
		if next < curr/2 { // jitter is ±20%, so never drop below half
			t.Fatalf("next %s collapsed below curr/2 from %s", next, curr)
		}
		curr = next
	}
}
