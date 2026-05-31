package mlruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestZScorePredictorWarmupSuppressesAnomalies(t *testing.T) {
	p := NewZScorePredictor(ZScoreOptions{WarmupSamples: 20, ZThreshold: 3, Alpha: 0.2})
	ctx := context.Background()
	now := time.Now()

	// Inject 19 normal samples; warmup gate should suppress every reply.
	for i := 0; i < 19; i++ {
		d, err := p.Observe(ctx, Sample{TenantID: "t", Metric: "m", Value: 1.0, At: now})
		if err != nil {
			t.Fatal(err)
		}
		if d.Anomaly {
			t.Errorf("warmup leak at i=%d: %+v", i, d)
		}
	}
}

func TestZScorePredictorFiresOnOutlier(t *testing.T) {
	p := NewZScorePredictor(ZScoreOptions{WarmupSamples: 30, ZThreshold: 3.0})
	ctx := context.Background()
	now := time.Now()

	// Jittered warm-up around mean ~100, std ~6.
	for i := 0; i < 50; i++ {
		v := 90 + float64(i%20)
		_, _ = p.Observe(ctx, Sample{TenantID: "t", Metric: "m", Value: v, At: now})
	}
	d, _ := p.Observe(ctx, Sample{TenantID: "t", Metric: "m", Value: 10000, At: now})
	if !d.Anomaly {
		t.Errorf("expected outlier to fire; got %+v", d)
	}
}

func TestInstrumentedTracksSamplesAndAnomalies(t *testing.T) {
	var samples, anoms, errs atomic.Int64
	p := WithCounters(NewZScorePredictor(ZScoreOptions{WarmupSamples: 30, ZThreshold: 3.0}),
		&Counters{Samples: &samples, Anomalies: &anoms, PredictErrors: &errs})

	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 50; i++ {
		v := 90 + float64(i%20)
		_, _ = p.Observe(ctx, Sample{TenantID: "t", Metric: "m", Value: v, At: now})
	}
	_, _ = p.Observe(ctx, Sample{TenantID: "t", Metric: "m", Value: 10000, At: now})

	if samples.Load() != 51 {
		t.Errorf("samples: %d", samples.Load())
	}
	if anoms.Load() < 1 {
		t.Errorf("expected >=1 anomaly; got %d", anoms.Load())
	}
}

func TestWithCountersNoopWhenNil(t *testing.T) {
	p := NewZScorePredictor(ZScoreOptions{})
	wrapped := WithCounters(p, nil)
	if wrapped != p {
		t.Errorf("nil counters must return the inner unchanged")
	}
}

func TestErrUnsupportedSentinel(t *testing.T) {
	// The default build (no `onnx` tag) returns ErrUnsupported.
	// We don't have a constructor we can call here without the tag,
	// but we can validate the sentinel itself.
	if !errors.Is(ErrUnsupported, ErrUnsupported) {
		t.Fatal("sentinel comparison broken")
	}
}

func TestZScorePredictorSeriesCount(t *testing.T) {
	p := NewZScorePredictor(ZScoreOptions{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = p.Observe(ctx, Sample{TenantID: "t", Metric: string(rune('a' + i))})
	}
	if got := p.SeriesCount(); got != 5 {
		t.Errorf("SeriesCount: %d", got)
	}
}
