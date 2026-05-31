# ADR-0012 — gRPC OTLP receiver

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3a

## Context

Phase B-2 (ADR-0005) implemented OTLP over HTTP. Most OTel SDKs
default to **gRPC** when given an OTLP exporter — Python, Go, Java,
JavaScript, .NET, and Rust all ship gRPC as the first-class
transport. Operators integrating ObserveX with stock SDK
configurations were forced to override the protocol explicitly, which
is a friction point and a common rollout mistake. The README has
advertised `:4317` as the gRPC port since Phase A, but the actual
gRPC server only exposed a private (non-OTLP) `IngestService.Export`
endpoint.

## Decision

Mount the three canonical OTLP gRPC services
(`opentelemetry.proto.collector.{trace,metrics,logs}.v1.{Trace,Metrics,Logs}Service`)
on the same `*grpc.Server` that already serves the legacy
`IngestService`. The OTLP services share the existing auth
interceptor; the interceptor now also enforces the `ingest` scope
introduced in ADR-0011.

The service implementations are intentionally thin: they re-marshal
the inbound protobuf message and feed it through the existing
`DecodeTraces` / `DecodeMetrics` / `DecodeLogs` decoders. This keeps
the wire-shape → `signal.Signal` projection in exactly one place;
any divergence between HTTP and gRPC paths would be a defect.

```
                   ┌─────────────────────────────────────────┐
SDK ─ gRPC ─ :4317 │ AuthInterceptor                          │
                   │  ├─ extracts Bearer from metadata        │
                   │  ├─ ValidateKeyWithMetadata              │
                   │  └─ HasScope(ingest)                     │
                   │                                          │
                   │  TraceService.Export ─ DecodeTraces  ─┐  │
                   │  MetricsService.Export ─ DecodeMetrics ┼─→ engine.ProcessSignal
                   │  LogsService.Export ─ DecodeLogs    ──┘  │
                   └─────────────────────────────────────────┘
```

Errors map to the canonical gRPC codes the OTel exporter spec
expects:

| Condition                       | gRPC code              |
|---------------------------------|------------------------|
| Missing/invalid bearer token    | `Unauthenticated` / `PermissionDenied` |
| Insufficient scope              | `PermissionDenied`     |
| Decode failure                  | `InvalidArgument`      |
| Engine back-pressure            | `ResourceExhausted`    |
| Internal remarshal failure      | `Internal`             |

`ResourceExhausted` is the canonical "retry with backoff" signal the
OTel gRPC exporter respects per
[OpenTelemetry Specification — OTLP/gRPC retries](https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/otlp.md#otlpgrpc-throttling).

## Trade-offs

- **Two server hosts on one port.** Both `IngestService` (legacy)
  and the three OTLP services live on the same `*grpc.Server`
  because (a) gRPC multiplexes naturally on the service-name prefix
  and (b) running two listeners would require an extra port mapping
  in the Helm chart / docker-compose. Acceptable; the only constraint
  is that the auth interceptor must be a no-op or pass-through for
  any caller already authenticated via the legacy path. It is, since
  it sets the same `x-tenant-id` metadata key.

- **Re-marshal cost.** Decoders take `[]byte`. We re-`proto.Marshal`
  the inbound request before handing it back to the decoder. This
  costs one extra allocation + serialisation per request, in the
  range of microseconds for typical payloads. The alternative —
  exposing a typed decode path
  (`DecodeTracesFromProto(*ExportTraceServiceRequest)`) — is a real
  follow-up if profiles show this is hot, but for now the simplicity
  of "one decoder" outweighs the cost.

- **No streaming RPCs.** All three OTLP services are unary today.
  The protocol does not (currently) define streaming Export, so this
  is fine.

- **No keepalive / load-shedding tuning.** Default grpc.ServerOptions
  with no `keepalive.EnforcementPolicy`. If we see misbehaving SDKs,
  we'll layer a `keepalive` policy and an `IPRateLimit` interceptor
  in a follow-up.

## Package changes

- `services/ingest-gateway/internal/otlp/grpc.go` (new):
  `traceService`, `metricsService`, `logsService`,
  `RegisterGRPCServices`, `AuthInterceptor`, `WithTenantForTest`.
- `services/ingest-gateway/internal/otlp/grpc_test.go` (new):
  happy-path + auth-failure + insufficient-scope tests with a real
  in-process grpc.Server.
- `services/ingest-gateway/internal/receiver/grpc_receiver.go`
  (modified): mounts the OTLP services next to the legacy
  IngestService; interceptor enforces the `ingest` scope and
  populates the OTLP tenant ctx key.

No new top-level dependencies — the OTLP gRPC service definitions
are already shipped by `go.opentelemetry.io/proto/otlp` (v1.10.0)
which we vendor for the HTTP decoder.

## Migration

For users already on OTLP/HTTP: nothing to do. For users on the
default gRPC exporter: change `OTEL_EXPORTER_OTLP_ENDPOINT` to
`http://<gateway>:4317` (note: still `http://` even though it's
gRPC — that's the OTel naming convention). No port changes; gRPC
on 4317 was already advertised in README.

## Verification

- `go test -race ./services/ingest-gateway/internal/otlp/...`
  exercises:
  - Export with valid `ingest`-scoped key → 200.
  - Export with no auth → `Unauthenticated`.
  - Export with `query`-only key → `PermissionDenied`.
- Manual smoke (left for the next operator): run
  `otel-collector-contrib` with the OTLP gRPC exporter pointed at
  `:4317`; payloads land in ClickHouse.
