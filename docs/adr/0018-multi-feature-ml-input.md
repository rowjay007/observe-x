# ADR-0018 — Multi-feature ML inference input

- Status: Accepted
- Date: 2026-06-01
- Phase: D-1

## Context

Phase C-3b shipped `pkg/mlruntime` with a `Predictor` that took a
single `Value float64` per observation. That was enough for the
default rolling z-score detector but blocks every real anomaly
model — Isolation Forest, multivariate LSTM, seasonality
decomposition — which all expect a feature vector per sample.

We need to lift the input shape without breaking the existing
single-feature contract that ZScorePredictor and the legacy
`internal/detector` rely on.

## Decision

Add an optional `Features []float64` to `mlruntime.Sample`:

```go
type Sample struct {
    TenantID string
    Metric   string
    Value    float64    // single-feature back-compat
    Features []float64  // optional multi-feature
    At       time.Time
}

func (s Sample) FeatureVector() []float64 { … }
```

Predictors call `FeatureVector()` rather than reading either field
directly. The helper returns `Features` when non-nil, otherwise
`[Value]`. Existing call sites (anomaly detector HTTP intake,
ZScorePredictor) keep working unchanged.

The ONNX adapter gains an `InputFeatures int` option (default 1).
It pads or truncates the input slice to that width — a dimension
mismatch would crash the session.

## Trade-offs

- **Back-compat over purity** — we could have replaced `Value`
  with `Features` and updated every caller, but the change would
  ripple into stream-processor, telemetry pipelines, and the WASM
  ABI. Carrying both fields costs 24 bytes per sample and is
  invisible at the API edge.
- **No type-tagged inputs (yet)** — every feature is a `float64`.
  Models that need categorical inputs (one-hot, embeddings) must
  pre-encode on the client. Future work: a typed `Feature` union
  if the model zoo grows.
- **No batched inference** — we keep one-sample-per-Observe for
  simplicity. The ONNX adapter holds a per-session `sync.Mutex` so
  the latency win from batching would be modest until we add a
  separate batched API.

## Package changes

- `pkg/mlruntime/mlruntime.go` — new fields + helper, new tests.
- `pkg/mlruntime/onnx_runtime.go` (build tag `onnx`) — input
  shape generalised, env var documented.
- `pkg/mlruntime/onnx_stub.go` — mirror the new options struct.
- `services/ml-anomaly-detector/cmd/main.go` — HTTP body accepts
  `features` field and forwards it to the predictor.

## Configuration

- `OBSERVE_X_ML_INPUT_FEATURES=4` — number of feature floats the
  model expects. Default 1.

## Verification

- `go test -race ./pkg/mlruntime/...` — `TestSampleFeatureVectorBackCompat`
  asserts both paths.
- Demo: send a 4-feature observation to the detector with an
  Isolation Forest ONNX model in single-sample mode.
