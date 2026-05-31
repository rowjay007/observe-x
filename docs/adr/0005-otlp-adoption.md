# ADR 0005 — OTLP/HTTP wire-format adoption

- **Status:** Accepted (Phase B-2)
- **Date:** 2026-05-29
- **Supersedes:** the Phase A byte-passthrough OTLP handler

## Context

Phase A accepted OTLP traces as opaque bytes and tagged them with a
single `source=otlp` attribute. The Engine couldn't distinguish a
500ms span from a healthcheck because span-level data lived inside
the (un-decoded) protobuf. Any real OpenTelemetry SDK pointed at the
gateway was "accepted" but produced useless ClickHouse rows.

## Decision

### Three standard OTLP/HTTP endpoints

```
POST /v1/traces      application/x-protobuf  (gzip optional)
POST /v1/metrics     application/x-protobuf  (gzip optional)
POST /v1/logs        application/x-protobuf  (gzip optional)
```

These are the paths every OTel SDK and the Collector know. We
deliberately mounted them at the same prefix as the spec so users do
not need a non-default OTLP exporter config. The legacy
`/v1/otlp/traces` alias remains for backward compatibility but is
documented as deprecated; planned removal in Phase C.

### Protobuf decoded into typed Signals

`services/ingest-gateway/internal/otlp/decoder.go` uses
`go.opentelemetry.io/proto/otlp` and unmarshals each request into
typed Go structs. Each request expands to **one Signal per data
point** (span, metric data-point, log record). Resource attributes
are flattened into every emitted Signal so downstream stages — CEP,
sampler, ClickHouse writer — never need OTLP-specific awareness.

Key attribute mappings:

| OTLP field                | Signal.Attributes key            |
|---------------------------|----------------------------------|
| Resource attributes       | (top level, e.g. `service.name`) |
| Scope name                | `otel.scope.name`                |
| Span.TraceId / SpanId     | `trace_id`, `span_id`            |
| Span.Status=ERROR         | `severity = ERROR`               |
| Span.EndTime-StartTime    | `duration_ms`                    |
| Metric.Name / Unit        | `metric_name`, `metric_unit`     |
| Sum/Gauge NumberDataPoint | `value`                          |
| Histogram                 | `count`, `sum`                   |
| LogRecord.SeverityText    | `severity`                       |
| LogRecord.Body            | Signal.Payload (raw bytes)       |

### Response codes follow the OTLP/HTTP spec

| Code | When                                                            |
|------|-----------------------------------------------------------------|
| 200  | Accepted in full — empty ExportXServiceResponse body             |
| 400  | Malformed protobuf, missing tenant header, gzip decode failure   |
| 413  | Body > 8 MiB (matches OTel Collector default)                    |
| 415  | Wrong content-type                                               |
| 429  | Engine back-pressure                                             |

429 specifically because the OTel SDK honours the OTLP/HTTP spec's
retry-with-backoff behaviour for that code; 503 would also work but
SDKs treat it as a hard failure by default.

### 8 MiB body cap

Hard-coded for now (matches the Collector default). Larger requests
should batch instead of stuffing a single payload. Tunable via
`MaxRequestBytes` constant; promoted to env config in Phase C when
deployment configs need it.

### Both ResourceXxx and XxxData wire shapes accepted

In the wild, SDKs sometimes send the bare `ResourceSpans` /
`ResourceMetrics` / `ResourceLogs` message instead of the wrapping
`TracesData` / `MetricsData` / `LogsData`. The decoder tries the
wrapper first and falls back to the bare form. Slight extra work on
the cold path, zero impact on the hot path.

## Consequences

### Positive

- Real OTel SDKs (otel-sdk-go, otel-sdk-python, otel-sdk-js, the
  collector) "just work" against the gateway.
- Trace correlation, severity-based sampling, latency-based sampling,
  and CEP error-rate rules can all key off real fields instead of the
  opaque payload.
- 8 MiB cap defends the WAL and the engine queue from a single client
  flooding the system with a giant batch.

### Negative

- Body cap is fixed — a deliberately oversized customer batch is 413
  even if the system has capacity. Acceptable; batching is correct.
- We don't yet emit OTLP partial-success responses (only "all
  accepted" or "none"). Real partial accept lands in B-3.5 when the
  engine's per-signal error channel is exposed.

### Deferred to Phase C

- gRPC OTLP receiver (`/opentelemetry.proto.collector.*.v1.*`).
  HTTP/protobuf covers 100% of SDKs we care about today and is
  diagnosable with curl + protoc; gRPC adds operational complexity
  (HTTP/2, ALPN, mTLS interop with OTel collectors) we don't need
  for Phase B.
- Partial-success body. Most SDKs don't read it; adding it is a
  non-breaking server-side change later.
