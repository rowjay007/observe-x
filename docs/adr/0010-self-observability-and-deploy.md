# ADR 0010 — Self-observability + deploy story

- **Status:** Accepted (Phase C-2)
- **Date:** 2026-05-31

## Context

A multi-tenant observability platform that can't observe itself is
operating blind. Phase B shipped per-service `/metrics` endpoints
exposing Prometheus counters, but:

1. No tracing across the platform's own RPC graph (e.g. "this slow
   query in the UI maps to that ClickHouse round-trip").
2. No defined deploy story — services built into binaries, but no
   container image policy, no Compose target for the full stack,
   no Helm chart, no scrape configuration to feed Prometheus.

Phase C-2 closes both: every service emits its own traces (via OTLP
loopback into the ingest-gateway), a real Helm chart exists, and a
full Docker Compose stack with Grafana/Prometheus pre-wired is the
new development entrypoint.

## Decision

### `pkg/selfobs` — OTel SDK convention layer

A thin wrapper over the OpenTelemetry Go SDK that:

- Reads `OBSERVE_X_OTLP_*` env vars and builds a `TracerProvider`
  with the OTLP/HTTP exporter pointed at the ingest-gateway.
- Uses `ParentBased(TraceIDRatioBased(fraction))` sampling — propagate
  upstream decisions when present, fall back to head-fraction
  otherwise. Default 0.10.
- Returns a NO-OP provider when `OBSERVE_X_OTLP_ENDPOINT` is unset.
  **Critical**: never crash a service for missing observability config.
- Sets the global propagator to W3C TraceContext + Baggage so every
  HTTP call automatically carries trace headers.

Each service main calls exactly:

```go
tp, _ := selfobs.InitFromEnv(ctx, "ingest-gateway", "1.0.0")
defer tp.Shutdown(...)
```

That's the entire wiring. Manual span creation lives in
per-service files using `otel.Tracer(...)` as usual.

### Loopback topology

Services trace back into themselves via the ingest-gateway's existing
`/v1/traces` endpoint (Phase B-2 OTLP receiver). This means:

- No new infrastructure is required — the OTel collector pattern is
  replaced by the ingest-gateway itself.
- Real OTel SDK clients work against ObserveX out of the box. The
  same code path that ingests application traces ingests platform
  traces.
- Authentication uses a dedicated low-privilege tenant + API key
  (`OBSERVE_X_OTLP_TENANT_ID`, `OBSERVE_X_OTLP_API_KEY`); the
  bootstrap script creates one called `observex-platform`.

### Deploy story

Three artifacts ship in `deploy/`:

#### `deploy/compose/docker-compose.yml`

Full local stack: ClickHouse + Postgres + Redis + NATS + Prometheus +
Grafana + EVERY ObserveX service. Built from `build/docker/Dockerfile`
using a service-build-arg pattern so we have one Dockerfile, not six.
Health-check-driven bring-up; volumes for WAL + Postgres + ClickHouse
data; Grafana pre-provisioned with the Prometheus datasource and the
"ObserveX — Platform Overview" dashboard.

#### `deploy/helm/observex/`

Real Helm chart (v2 API) that renders Deployments + Services +
optional ServiceMonitors for all five services. Infra (Postgres,
ClickHouse, Redis, NATS) is **intentionally not bundled**: real
operators always have an opinion about how those run (cloud-managed
RDS, dedicated operators, etc.). Bundling them invites surprise.

Layout:

```
deploy/helm/observex/
  Chart.yaml          # apiVersion v2, version 0.1.0, appVersion 1.0.0
  values.yaml         # per-service replica / resources / image / persistence
  templates/
    _helpers.tpl              # labels, image refs, common env
    ingest-gateway.yaml       # Deployment + Service + PVC + ServiceMonitor
    query-engine.yaml         # ditto
    tenant-api.yaml           # ditto
    alert-manager.yaml        # ditto
    ml-anomaly-detector.yaml  # ditto
```

Secrets (Postgres DSN, admin tokens, Slack/PagerDuty keys) come from
an existing Kubernetes Secret named in `values.yaml`. We never
template secret values directly into manifests.

Lint clean (`helm lint deploy/helm/observex`); renders to 530+ lines
of valid Kubernetes YAML.

#### `build/docker/Dockerfile`

Single multi-stage Dockerfile parameterized by `SERVICE` build arg.
Builds against `golang:1.25-alpine`, runs on `distroless/static:nonroot`.
Images land at ~15 MiB each, no shell, no libc, no package manager.

## Trade-offs

**OTLP loopback vs sidecar collector.** A dedicated OTel Collector
sidecar (or DaemonSet) is the conventional pattern. We skip it because:

- The ingest-gateway *is* an OTLP collector. Adding a second one is
  redundant complexity.
- One fewer pod per node = lower baseline resource cost.
- We lose the collector's processors (batch, attributes, tail
  sampling) at this layer — but the same processors are already in
  the ingest-gateway's pipeline stages. Net wash.

If we later need the collector's full processor catalog at the
service edge, we can drop one in as a sidecar without touching the
SDK config — `OBSERVE_X_OTLP_ENDPOINT` just points at the sidecar
instead of the gateway.

**Helm chart does not bundle infra.** Discussed above. Trade-off: one
more step in "deploy ObserveX from scratch." Mitigation: the Compose
stack in `deploy/compose/` is the one-command local entry; the Helm
chart's README will be explicit about the expected infra topology.

**Single Dockerfile, multiple services.** Each service build re-runs
`go mod download`; using a multi-output build (`go build ./...`) into
multiple final stages would be faster for CI. We optimise for
simplicity first; CI build time is currently dominated by tests, not
image builds.

**No ArgoCD `Application` resource yet.** The Helm chart is the
interface; how you GitOps it (Argo / Flux / hand-`helm install`) is
operator choice. We'll add an example Argo `Application` in Phase C-3
once we have a reference cluster to test it against.

## Package changes

| Package / path                         | Change |
|----------------------------------------|--------|
| `pkg/selfobs/`                         | NEW. OTel SDK convention layer. |
| `services/*/cmd/main.go` × 5           | Additive. Single-line `selfobs.InitFromEnv` at top of main. |
| `build/docker/Dockerfile`              | NEW. Multi-stage, SERVICE arg, distroless/static runtime. |
| `deploy/compose/docker-compose.yml`    | NEW. Full local stack including Prom + Grafana. |
| `deploy/prometheus/prometheus.yml`     | NEW. Scrapes all five services. |
| `deploy/grafana/dashboards/`           | NEW. "Platform Overview" dashboard JSON + provisioning. |
| `deploy/helm/observex/`                | NEW. v2 chart, lint clean. |

## Deferred to Phase C-3+

- Real ArgoCD `Application` + `ApplicationSet` examples.
- gRPC OTLP receiver in ingest-gateway (currently HTTP-only).
- Service-mesh integration (Istio/Linkerd headers).
- Tail sampling at the gateway (currently head-only).
- Cold tier (S3+Parquet) + audit-log export.
- UI (React + D3).
- Operator OIDC for tenant-api admin endpoints.
- Read/write scopes on tenant API keys.
