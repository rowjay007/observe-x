# ADR-0016 — Pluggable ML inference runtime

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3b

## Context

Phase B-5 shipped `ml-anomaly-detector` with a single hard-wired
rolling z-score detector. It works for the easy 90% case — pick a
metric, flag samples that deviate from the EWMA baseline — but it
falls down on the harder problems customers actually have:

- **Seasonality.** RPS for a SaaS product has weekday/weekend and
  hour-of-day patterns; z-score on a flat baseline cries wolf every
  Monday morning and misses anomalies during weekend lulls.
- **Multi-feature signals.** A real "is this service unhealthy"
  classifier wants RPS, error rate, p99 latency, and saturation in
  the same input vector. z-score on each in isolation loses
  cross-correlation.
- **Customer-specific models.** Sophisticated tenants want to bring
  their own Isolation Forest / LSTM / Prophet — trained on their
  own data, encoding their own business logic, version-controlled
  in their own repo.

We deliberately do NOT want to:

- Build our own ML training stack. That's a 10-person team's problem
  and there are excellent open-source projects (Prophet, Kats,
  Merlion, etc.) already doing it well.
- Embed a heavyweight model server (Triton, KServe). They make the
  default deployment 2 GB heavier and add operational complexity
  for the 90% of tenants who never need it.
- Hard-require CGo. A vanilla `go build` of ObserveX must produce
  a working binary.

## Decision

Introduce `pkg/mlruntime` as a **predictor seam**:

```go
type Predictor interface {
    Name() string
    Observe(ctx, Sample) (Decision, error)
    Close() error
}
```

with two implementations:

1. **`ZScorePredictor`** (default) — the existing Welford-style
   EWMA detector, moved to live behind the seam unchanged. Pure
   Go, no CGo, no model files. Selected by the empty value (or
   the explicit string `"zscore"`) of `OBSERVE_X_ML_MODEL`.
2. **`OnnxPredictor`** (opt-in) — loads a `.onnx` model file via
   `github.com/yalue/onnxruntime_go`. Selected by
   `OBSERVE_X_ML_MODEL=onnx` plus `OBSERVE_X_ML_MODEL_PATH`.
   Compiled in only when built with `-tags onnx`; the default
   build returns `ErrUnsupported` for that selection so the
   operator gets a startup-time error rather than a silent
   downgrade.

`Instrumented` wraps any predictor with `Counters` (atomic.Int64
hooks) so the consuming service can attach Prometheus metrics
without `mlruntime` taking a hard dep on `prometheus/client_golang`.

`ml-anomaly-detector` was rewritten to use the seam:
`buildPredictor()` reads the env, picks a predictor, and the rest of
the service is implementation-agnostic.

## Trade-offs

- **CGo is operator-opt-in.** The `onnx` build tag means operators
  who want ONNX must (a) `go get github.com/yalue/onnxruntime_go`,
  (b) install `libonnxruntime.{so,dylib,dll}` on the binary's load
  path, (c) build with `-tags onnx`. That's three things, but each
  one is honest about the trade — CGo is a real operational burden
  and we make it visible rather than hiding it.

- **Single-feature input today.** The ONNX adapter currently passes
  `Sample.Value` as a `[1, 1]` float32 tensor. Multi-feature input
  (RPS, error rate, latency in one inference) is a `Sample` shape
  change that we defer until a tenant actually asks for it; right
  now the shape decision would be premature.

- **Per-session mutex on ONNX.** `onnxruntime` sessions are not
  goroutine-safe. We serialise. For high-QPS workloads (>10k
  inferences/sec per pod) operators scale horizontally. We
  evaluated a session pool but rejected it for this slice — adds
  complexity and pool sizing is an additional knob the operator
  doesn't currently need.

- **No model versioning / hot-swap today.** The model is loaded
  at startup. To update, redeploy. ArgoCD makes this trivial; a
  hot-swap mechanism would need its own change-event protocol and
  isn't worth it at this maturity level.

- **No model registry integration.** Operators can mount a model
  file via a ConfigMap or pull from S3 in an init container. Wiring
  to MLflow / DVC / Sagemaker registries is straightforward via
  an init container; we don't take a position on which registry to
  prefer.

- **Decision shape is a deliberate constraint.** We chose
  `{Anomaly bool, Score, Baseline, Threshold float64}` over a free
  `map[string]any` so the downstream consumers (Prometheus, alert
  routing, audit logs) can rely on a stable field set. Models that
  emit richer output (e.g. SHAP attributions) drop them on the
  floor today; ADR-flagged for follow-up if the demand materialises.

## Package changes

- `pkg/mlruntime/mlruntime.go` (new): seam — `Predictor`, `Sample`,
  `Decision`, `Counters`, `WithCounters`, `ErrUnsupported`.
- `pkg/mlruntime/zscore.go` (new): default in-process Welford EWMA.
- `pkg/mlruntime/onnx_stub.go` (new, `!onnx` build): returns
  `ErrUnsupported`.
- `pkg/mlruntime/onnx_runtime.go` (new, `onnx` build): loads a model
  via `onnxruntime_go`, scores one sample at a time.
- `pkg/mlruntime/mlruntime_test.go` (new): predictor + instrumented
  + counters + sentinel coverage.
- `services/ml-anomaly-detector/cmd/main.go` (modified): swap
  `detector.New(...)` for `buildPredictor(...)`; instrumentation
  wired via `WithCounters`. The legacy `internal/detector` package
  stays unchanged so its tests still pass (and so operators who
  fork can find the canonical reference algorithm).

`onnx_runtime.go` references `github.com/yalue/onnxruntime_go`,
which is only required for the `-tags onnx` build. The default
build's `go.mod` is unchanged.

## Environment

```
OBSERVE_X_ML_MODEL                "" | zscore | onnx
OBSERVE_X_ML_MODEL_PATH           /models/iforest.onnx        (onnx mode)
OBSERVE_X_ML_MODEL_LIB            /opt/ort/libonnxruntime.so  (optional)
OBSERVE_X_ML_INPUT_NAME           float_input                 (onnx)
OBSERVE_X_ML_SCORE_THRESHOLD      0.5                         (onnx)

OBSERVE_X_ANOMALY_WARMUP          50    (zscore)
OBSERVE_X_ANOMALY_Z               3.0   (zscore)
OBSERVE_X_ANOMALY_ALPHA           0.05  (zscore)
```

## Alternatives considered

- **Embed a Python sidecar with FastAPI + scikit-learn.**
  Most flexibility, worst latency (HTTP-per-inference), and an extra
  language in the deployment graph. Rejected.

- **Use `gorgonia/gorgonia` for pure-Go tensor ops.** Pure Go, no
  CGo, but coverage of ONNX ops is partial and the import graph is
  large. We'd be locked in to whatever subset of model architectures
  Gorgonia happens to support. ONNX runtime via CGo is the
  industry-standard way to deploy ONNX in production today, and
  the build tag opt-in keeps the default build clean.

- **Triton Inference Server as a sidecar.** Excellent for big
  models, but the operational overhead (model repo, gRPC client,
  health checks, GPU scheduling) is dramatic for a feature
  designed to be optional. Rejected for the default path; nothing
  in this design prevents an operator from running Triton and
  pointing a custom `Predictor` implementation at it via a
  `PredictorURL` field in a future iteration.

## Verification

- `go test -race ./pkg/mlruntime/...` — predictor warmup gate,
  outlier firing, instrumented counters, sentinel comparison.
- `go test -race ./services/ml-anomaly-detector/...` — the legacy
  detector tests continue to pass; the service builds and runs
  with the new seam.
- `go build -tags onnx ./pkg/mlruntime/...` — only on machines
  with `onnxruntime_go` installed; not run in CI by default
  (documented in `.github/workflows/ci.yml`).
