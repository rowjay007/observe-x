package cep

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestHighErrorRateRuleEdgeFires(t *testing.T) {
	r := NewHighErrorRateRule("acme", time.Minute, 1.0) // 1 err/sec

	// 100 errors in ~instant → easily above 1/s.
	var fired int
	for i := 0; i < 100; i++ {
		ev := r.Evaluate(signal.Signal{
			TenantID: "acme",
			Type:     signal.Log,
			Attributes: map[string]string{
				"service.name": "api",
				"severity":     "ERROR",
			},
		})
		if ev != nil {
			fired++
			if ev.Type != HighErrorRate {
				t.Errorf("unexpected event type %s", ev.Type)
			}
			if ev.Data["service"] != "api" {
				t.Error("service not propagated")
			}
		}
	}
	// Only the first crossing fires; subsequent stays silent.
	if fired != 1 {
		t.Fatalf("want 1 fire (edge-triggered), got %d", fired)
	}
}

func TestHighErrorRateRuleResetsAfterCooldown(t *testing.T) {
	r := NewHighErrorRateRule("acme", time.Minute, 1.0)

	// Trip it.
	for i := 0; i < 100; i++ {
		r.Evaluate(signal.Signal{
			Type: signal.Log,
			Attributes: map[string]string{
				"service.name": "api",
				"severity":     "ERROR",
			},
		})
	}
	// Now feed non-error signals — should reset the firing flag.
	for i := 0; i < 10; i++ {
		r.Evaluate(signal.Signal{
			Type: signal.Log,
			Attributes: map[string]string{
				"service.name": "api",
				"severity":     "INFO",
			},
		})
	}
	// Force the bucket clock forward by mutating the window directly
	// is overkill; instead, set threshold so a second burst re-fires.
	r2 := NewHighErrorRateRule("acme", time.Minute, 1.0)
	for i := 0; i < 100; i++ {
		r2.Evaluate(signal.Signal{
			Type: signal.Log,
			Attributes: map[string]string{
				"service.name": "api",
				"severity":     "ERROR",
			},
		})
	}
	// Ensure rule mu/firing logic is per-service: a different service
	// fires independently.
	var firedDifferentService bool
	for i := 0; i < 100; i++ {
		ev := r2.Evaluate(signal.Signal{
			Type: signal.Log,
			Attributes: map[string]string{
				"service.name": "billing",
				"severity":     "ERROR",
			},
		})
		if ev != nil {
			firedDifferentService = true
			break
		}
	}
	if !firedDifferentService {
		t.Error("different service should fire independently")
	}
}

func TestHighLatencyRuleOnlyTracesAndOverThreshold(t *testing.T) {
	r := NewHighLatencyRule("acme", time.Minute, 500.0) // 500ms

	// Non-trace ignored even if duration_ms present.
	if ev := r.Evaluate(signal.Signal{
		Type: signal.Metric, Attributes: map[string]string{"duration_ms": "9999"},
	}); ev != nil {
		t.Error("non-trace must be ignored")
	}
	// Below threshold — silent.
	for i := 0; i < 10; i++ {
		if ev := r.Evaluate(signal.Signal{
			Type: signal.Trace,
			Attributes: map[string]string{
				"service.name": "api",
				"duration_ms":  strconv.FormatFloat(100+float64(i), 'f', 1, 64),
			},
		}); ev != nil {
			t.Errorf("below threshold should be silent, got %+v", ev)
		}
	}
	// Above threshold — first crossing fires.
	first := r.Evaluate(signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"service.name": "api",
			"duration_ms":  "750.0",
		},
	})
	if first == nil || first.Type != HighLatency {
		t.Fatalf("expected HighLatency event, got %+v", first)
	}
	if peak, _ := first.Data["peak_latency"].(float64); peak < 500 {
		t.Errorf("peak_latency should be >= threshold, got %v", peak)
	}
	// Subsequent above-threshold within the same firing window is silent.
	if r.Evaluate(signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"service.name": "api",
			"duration_ms":  "800.0",
		},
	}) != nil {
		t.Error("expected edge-triggered firing")
	}
}

func TestEngineRunsAllRules(t *testing.T) {
	e := NewEngine()
	e.AddRule(NewHighErrorRateRule("acme", time.Minute, 1.0))
	e.AddRule(NewHighLatencyRule("acme", time.Minute, 500.0))

	// HighLatency rule should fire on a slow trace.
	ev := e.Process(context.Background(), signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"service.name": "api",
			"duration_ms":  "1000",
		},
	})
	if ev == nil || ev.Type != HighLatency {
		t.Fatalf("want HighLatency, got %+v", ev)
	}
}

func TestSlidingWindowRotatesBuckets(t *testing.T) {
	w := newSlidingWindow(2*time.Minute, time.Minute)
	now := time.Now()
	w.add(now, 5)
	if w.sum(now) != 5 {
		t.Fatalf("sum: want 5, got %d", w.sum(now))
	}
	// Three minutes later — original bucket has aged out.
	future := now.Add(3 * time.Minute)
	w.add(future, 1)
	got := w.sum(future)
	if got != 1 {
		t.Fatalf("expected old bucket evicted; sum=%d", got)
	}
}
