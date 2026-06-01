// Package mlruntime is ObserveX's anomaly-detection abstraction.
//
// Phase B-5 shipped a single hard-coded rolling z-score detector
// (services/ml-anomaly-detector/internal/detector). That's a great
// default — it's online, it's O(1) per sample, and it catches the
// 99%-of-people-need-this case. But real customers eventually want:
//
//   - their own forecast model (Isolation Forest / LSTM / Prophet) so
//     they can encode seasonality + business context;
//   - the same model in dev, staging, and prod, version-controlled;
//   - a no-CGo default so a vanilla `go build` still produces a
//     working binary.
//
// `mlruntime.Predictor` is the seam. The default `ZScorePredictor`
// wraps the existing detector unchanged. The `OnnxPredictor` adapter
// loads a `.onnx` model file at startup and runs inference per
// observation; it is opt-in behind the `onnx` build tag because it
// requires the operator to ship libonnxruntime.so (a CGo dep we are
// not going to vendor into the default build).
//
// The selection logic is env-driven: `OBSERVE_X_ML_MODEL=onnx` plus
// `OBSERVE_X_ML_MODEL_PATH=/models/iforest.onnx` flips on the ONNX
// path on builds compiled with `-tags onnx`. Anything else, or any
// build without the tag, uses the z-score default.
//
// The `Decision` shape is deliberately the same as the existing
// detector's Anomaly type so downstream consumers (Prometheus
// counters, alert-manager publication) don't have to fork.
package mlruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Sample is one (tenant, metric, value, time) tuple to score.
//
// Phase D-1 generalised this from a single Value to an optional
// Features vector to support multi-feature models (Isolation
// Forest, multivariate LSTM, etc.). Single-feature predictors
// continue to read `Value`; multi-feature predictors read
// `Features` and fall back to `[Value]` when Features is nil.
// See ADR-0018.
type Sample struct {
	TenantID string
	Metric   string
	Value    float64   // legacy single-feature input; mirrored to Features[0] on the wire
	Features []float64 // optional multi-feature input; nil ⇒ use [Value]
	At       time.Time
}

// FeatureVector returns Features when non-nil, otherwise [Value].
// Predictors should call this rather than reading either field
// directly so the back-compat shim stays one place.
func (s Sample) FeatureVector() []float64 {
	if s.Features != nil {
		return s.Features
	}
	return []float64{s.Value}
}

// Decision is the predictor's verdict. Anomaly == false means the
// sample is within tolerance; the other fields are populated for
// observability either way.
type Decision struct {
	Anomaly   bool
	Score     float64 // model-defined; z-score for the default
	Baseline  float64 // model-defined; rolling mean for the default
	Threshold float64
}

// Predictor is the inference seam. Implementations MUST be safe for
// concurrent callers. Observe both records the sample for the
// online learners (z-score) AND scores it; pure-inference models
// can return Decision{} for samples used only as training context.
type Predictor interface {
	Name() string
	Observe(ctx context.Context, s Sample) (Decision, error)
	// Close releases any held resources (file handles, runtime
	// sessions). Idempotent.
	Close() error
}

// ErrUnsupported is returned by adapters whose underlying runtime
// wasn't compiled in (e.g. ONNX adapter on a non-tag build).
var ErrUnsupported = errors.New("mlruntime: predictor unsupported in this build")

// ─── Observability counters wired by the consuming service ───────────────

// Counters is the optional hook so the consuming service can attach
// Prometheus metrics without mlruntime taking a hard dep on
// prometheus/client_golang. The consuming service constructs the
// counters in its main() and threads them through.
type Counters struct {
	Samples       *atomic.Int64
	Anomalies     *atomic.Int64
	PredictErrors *atomic.Int64
}

// Instrumented wraps a Predictor with the Counters. It's intentionally
// a wrapper rather than an option on each impl so consumers can swap
// metric backends without touching the predictor implementations.
type Instrumented struct {
	inner    Predictor
	counters *Counters
}

func WithCounters(p Predictor, c *Counters) Predictor {
	if c == nil {
		return p
	}
	return &Instrumented{inner: p, counters: c}
}

func (i *Instrumented) Name() string { return i.inner.Name() }
func (i *Instrumented) Observe(ctx context.Context, s Sample) (Decision, error) {
	if i.counters.Samples != nil {
		i.counters.Samples.Add(1)
	}
	d, err := i.inner.Observe(ctx, s)
	if err != nil {
		if i.counters.PredictErrors != nil {
			i.counters.PredictErrors.Add(1)
		}
		return d, err
	}
	if d.Anomaly && i.counters.Anomalies != nil {
		i.counters.Anomalies.Add(1)
	}
	return d, nil
}
func (i *Instrumented) Close() error { return i.inner.Close() }
