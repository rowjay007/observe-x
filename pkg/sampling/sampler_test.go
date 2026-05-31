package sampling

import (
	"context"
	"strconv"
	"testing"

	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestAdaptiveSamplerKeepsHighScoreTrace(t *testing.T) {
	sampler := NewAdaptiveSampler(0.1, 2)

	sig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id": "trace-high",
			"severity": "ERROR",
		},
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected high-score trace to be kept")
	}
}

func TestAdaptiveSamplerKeepsRecentTrace(t *testing.T) {
	sampler := NewAdaptiveSampler(0.1, 2)

	sig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id":    "trace-recent",
			"duration_ms": "2000",
		},
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected first trace to be kept")
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected second recent trace to be kept")
	}
}

func TestEWMAZScoreBoostsLatencyOutlier(t *testing.T) {
	s := NewAdaptiveSampler(0.1, 100)
	// Warm the baseline past the cold-start gate (30 samples) with
	// natural-looking jitter so variance is non-zero.
	for i := 0; i < 60; i++ {
		dur := 80 + float64(i%40) // 80–120ms, mean ≈100, std ≈12
		s.Score(signal.Signal{
			Type: signal.Trace,
			Attributes: map[string]string{
				"trace_id":     "t-" + strconv.Itoa(i),
				"service.name": "api",
				"duration_ms":  strconv.FormatFloat(dur, 'f', 1, 64),
			},
		})
	}
	outlier := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id":     "outlier",
			"service.name": "api",
			"duration_ms":  "5000",
		},
	}
	if got := s.Score(outlier); got <= 50 {
		t.Fatalf("EWMA outlier should add z-score boost on top of the >1s bump; got %f", got)
	}
}

func TestParentSampledFlagAddsPriority(t *testing.T) {
	s := NewAdaptiveSampler(0.01, 10)
	low := s.Score(signal.Signal{Type: signal.Trace, Attributes: map[string]string{"trace_id": "x"}})
	high := s.Score(signal.Signal{Type: signal.Trace, Attributes: map[string]string{"trace_id": "y", "parent_sampled": "true"}})
	if high <= low {
		t.Fatalf("parent_sampled should bump score: low=%f high=%f", low, high)
	}
}

func TestStateStoreRoundTrip(t *testing.T) {
	mem := NewInMemoryStore()

	s1 := NewAdaptiveSamplerWithOptions(SamplerOptions{
		SamplingRate: 0.1, MaxSize: 100,
		TenantID: "acme", State: mem,
	})
	for i := 0; i < 60; i++ {
		dur := 80 + float64(i%40)
		s1.Score(signal.Signal{
			Type: signal.Trace,
			Attributes: map[string]string{
				"service.name": "api",
				"duration_ms":  strconv.FormatFloat(dur, 'f', 1, 64),
			},
		})
	}
	// Force a flush via Close.
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// New sampler should hydrate the EWMA tracker from the store.
	s2 := NewAdaptiveSamplerWithOptions(SamplerOptions{
		SamplingRate: 0.1, MaxSize: 100,
		TenantID: "acme", State: mem,
	})
	defer s2.Close()

	// With baseline ≈100ms loaded, a 5s trace should pick up z-score boost.
	z := s2.latency.zscore("api", 5000.0)
	if z <= 0 {
		t.Fatalf("expected positive z-score from restored baseline; got %f", z)
	}
	if loaded, _ := mem.Load(context.Background(), "acme"); len(loaded) == 0 {
		t.Fatal("store should still hold snapshot after restart")
	}
}

func TestInMemoryStoreIsolatesTenants(t *testing.T) {
	mem := NewInMemoryStore()
	_ = mem.Save(context.Background(), "a", map[string]ewmaSnapshot{"svc": {Mean: 1, Variance: 1, Count: 50}})
	_ = mem.Save(context.Background(), "b", map[string]ewmaSnapshot{"svc": {Mean: 9, Variance: 1, Count: 50}})
	a, _ := mem.Load(context.Background(), "a")
	b, _ := mem.Load(context.Background(), "b")
	if a["svc"].Mean == b["svc"].Mean {
		t.Fatal("tenants must not share state")
	}
}
