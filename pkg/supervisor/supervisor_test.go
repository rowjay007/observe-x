package supervisor

import (
	"context"
	"errors"
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

// Phase D-7: full mailbox + spillover succeeds ⇒ spilled++, dropped untouched.
type fakeSpiller struct {
	calls atomic.Int64
	fail  bool
}

func (f *fakeSpiller) Push(_ context.Context, _ string, _ signal.Signal) error {
	f.calls.Add(1)
	if f.fail {
		return errors.New("simulated")
	}
	return nil
}

func TestRouteSpillsWhenMailboxFull(t *testing.T) {
	sp := &fakeSpiller{}
	sup := NewSupervisorWithOptions(Options{
		MailboxSize: 1,
		Spillover:   sp,
	})
	sup.Start()
	defer sup.Stop()

	// Don't start a real actor draining; the first signal fills the
	// mailbox, the second should spill.
	tenant := "press"
	sup.RouteToTenant(tenant, signal.Signal{TenantID: tenant, Type: signal.Log})
	sup.RouteToTenant(tenant, signal.Signal{TenantID: tenant, Type: signal.Log})

	// Wait for actor processing to enqueue.
	time.Sleep(50 * time.Millisecond)

	st := sup.Stats()
	if sp.calls.Load() < 1 {
		t.Errorf("spiller never invoked: %d calls", sp.calls.Load())
	}
	// dropped should still be 0 because spillover succeeded.
	if st.TotalDropped != 0 {
		t.Errorf("dropped should be 0 when spillover succeeds, got %d", st.TotalDropped)
	}
}

func TestRouteFallsBackToDropOnSpillerError(t *testing.T) {
	sp := &fakeSpiller{fail: true}
	sup := NewSupervisorWithOptions(Options{
		MailboxSize: 1,
		Spillover:   sp,
	})
	sup.Start()
	defer sup.Stop()

	tenant := "press2"
	sup.RouteToTenant(tenant, signal.Signal{TenantID: tenant, Type: signal.Log})
	sup.RouteToTenant(tenant, signal.Signal{TenantID: tenant, Type: signal.Log})

	time.Sleep(50 * time.Millisecond)

	st := sup.Stats()
	if st.TotalDropped == 0 {
		t.Errorf("expected drop when spillover errors")
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
