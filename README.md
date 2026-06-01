# observe-x

Distributed observability and APM platform written in Go. Self-hosted,
multi-tenant ingestion, processing, storage, and query engine for
metrics, logs, traces, and profiling data.

> **Status — v1.0 production-ready (Phase D shipped, Phase E-0 visualization live).**
> Phase A + Phase B + Phase C (slices 1, 2, 3a, 3b, 4) + Phase D
> (slices 1‑10) are all complete. Phase E-0 adds the visualization
> stack ([ADR-0028](./docs/adr/0028-visualization-strategy.md)):
> a provisioned Grafana ClickHouse datasource and three tenant-facing
> dashboards (`tenant-metrics`, `tenant-logs`, `tenant-traces`) under
> the `ObserveX / Tenant` folder, alongside the existing
> `ObserveX / Platform` self-observability dashboard. A native
> visualization workbench inside `services/ui-server` is queued as
> Phase E-1..E-4 (see [roadmap.md](./roadmap.md)).
>
> Phase D adds:
>
> - **Multi-feature ML inference** via `Sample.Features []float64`
>   ([ADR-0018](./docs/adr/0018-multi-feature-ml-input.md));
> - **Per-tenant cold-tier retention** through
>   `PUT /v1/tenants/:id/retention` on tenant-api
>   ([ADR-0019](./docs/adr/0019-per-tenant-retention.md));
> - **Live alert SSE stream** at `GET /v1/alerts/stream`
>   ([ADR-0020](./docs/adr/0020-alert-sse-stream.md));
> - **Operator audit aggregation** UI tab + `GET /v1/audit`
>   ([ADR-0021](./docs/adr/0021-operator-audit-aggregation.md));
> - **Browser PKCE OIDC** sign-in for the operator console
>   ([ADR-0022](./docs/adr/0022-pkce-oidc-browser.md));
> - **Arrow IPC** codec for `POST /v1/query` via content
>   negotiation ([ADR-0023](./docs/adr/0023-arrow-ipc-codec.md));
> - **NATS JetStream spillover** for the supervisor — bounded
>   mailbox + durable side-car
>   ([ADR-0024](./docs/adr/0024-nats-spillover.md));
> - **Parquet cold-tier export** from query-engine
>   ([ADR-0025](./docs/adr/0025-parquet-cold-tier-export.md));
> - **CBO skeleton** in `pkg/observeql`
>   ([ADR-0026](./docs/adr/0026-observeql-cbo.md));
> - **Federated S3 + DuckDB executor seam**
>   ([ADR-0027](./docs/adr/0027-federated-duckdb-executor.md)).
>
> Phase C still ships everything below:
>
> - **Operator OIDC** in front of every tenant-api admin endpoint
>   (`pkg/oidc` — JWKS auto-refresh, group-allowlist RBAC, fails
>   closed if both OIDC and the legacy admin token are configured
>   — [ADR-0014](./docs/adr/0014-operator-oidc.md)),
> - **S3 cold tier** for ClickHouse — multi-disk `hot_cold` storage
>   policy + `TTL ... TO DISK 'cold_s3'` lifecycle, plus
>   `services/cold-tier-controller` for per-disk Prometheus gauges
>   ([ADR-0015](./docs/adr/0015-cold-tier.md)),
> - a **pluggable ML runtime** (`pkg/mlruntime.Predictor`): z-score
>   default + ONNX adapter behind `-tags onnx`
>   ([ADR-0016](./docs/adr/0016-ml-runtime.md)),
> - the **operator UI** at `services/ui-server` — a single Go binary
>   that `embed.FS`-bundles a vanilla-JS SPA and reverse-proxies
>   `/api/*` to tenant-api / query-engine / alert-manager with OIDC
>   validation at the boundary ([ADR-0017](./docs/adr/0017-ui-server.md)).
>
> See [`docs/adr/`](./docs/adr) for the seventeen ADRs and
> [`roadmap.md`](./roadmap.md) for the full milestone map.

## Quick start

### Prerequisites

- Go **1.25+** (bumped in Phase B-1 because `jackc/pgx/v5` requires
  it — see ADR-0004).
- Docker + Docker Compose for local ClickHouse, Postgres, Redis, NATS.

### Run locally (production-shape: real Postgres-backed keys)

```bash
docker compose -f tests/docker-compose.yml up -d postgres clickhouse

export OBSERVE_X_POSTGRES_URL="postgres://observex:observex@localhost:5432/observex?sslmode=disable"
export OBSERVE_X_TENANT_API_ADMIN_TOKEN="$(openssl rand -hex 32)"
export OBSERVE_X_WAL_PATH=/tmp/observex/wal
export OBSERVE_X_CLICKHOUSE_ADDR=localhost:9000
export OBSERVE_X_CLICKHOUSE_DB=observex

# 1) Start the control plane (applies migrations on first start).
go run ./services/tenant-api/cmd &

# 2) Start the ingest gateway (uses the same Postgres as a read-only
#    KeyStore consumer; falls back to dev mode only if POSTGRES_URL is unset).
go run ./services/ingest-gateway/cmd &
```

Mint your first tenant + key against the control plane:

```bash
ADMIN="Authorization: Bearer ${OBSERVE_X_TENANT_API_ADMIN_TOKEN}"

# create tenant
curl -s -X POST http://localhost:7400/v1/tenants \
  -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"id":"acme","display_name":"Acme Corp","tier":"pro"}'

# issue an API key — the response shows the raw key ONCE.
# Phase C-3a: pass `scopes` to bake a least-privilege key. Omit it
# for the default (`["ingest"]`).
KEY=$(curl -s -X POST http://localhost:7400/v1/tenants/acme/api-keys \
  -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"scopes":["ingest","query"]}' | jq -r .raw_key)

# ingest with the new key
curl -X POST http://localhost:4318/v1/ingest \
  -H "Authorization: Bearer ${KEY}" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id":"acme","type":"METRIC",
       "payload":"eyJ2YWx1ZSI6MX0=",
       "attributes":{"metric_name":"requests"}}'

# list keys (metadata only — raw key never returned again)
curl -s -H "$ADMIN" http://localhost:7400/v1/tenants/acme/api-keys | jq

# revoke a key
KID=$(echo "$KEY" | cut -d: -f2)
curl -s -X DELETE -H "$ADMIN" http://localhost:7400/v1/tenants/acme/api-keys/$KID
```

### Run locally (dev-only: single shared secret, no Postgres)

```bash
export OBSERVE_X_API_SECRET="dev-only-secret"   # see ADR-0003 — DEV ONLY
export OBSERVE_X_WAL_PATH=/tmp/observex/wal

go run ./services/ingest-gateway/cmd
```

> **Security note.** The single-secret `StatelessKeyValidator` is
> **dev only**. Any leak of `OBSERVE_X_API_SECRET` lets an attacker
> mint a valid key for every tenant. Production deployments MUST set
> `OBSERVE_X_POSTGRES_URL`; the gateway then refuses to start in
> dev-mode and uses `PostgresKeyStore` (per-tenant Argon2id keys,
> revocation, 5s-TTL cache). See ADR-0003 and ADR-0004.

### Services and ports

| Service              | Port | Endpoints                                                                                      |
|----------------------|------|------------------------------------------------------------------------------------------------|
| ingest-gateway HTTP  | 4318 | `/v1/ingest`, **`/v1/traces`**, **`/v1/metrics`**, **`/v1/logs`** (OTLP/protobuf, gzip optional), `/health`, `/ready`, `/metrics` |
| ingest-gateway gRPC  | 4317 | OTLP-shaped `IngestService.Export`                                                             |
| ingest-gateway UDP   | 8125 | StatsD                                                                                         |
| tenant-api           | 7400 | `/v1/tenants`, `/v1/tenants/:id/api-keys`, `/health`, `/metrics`                               |
| query-engine         | 7500 | `POST /v1/query` (ObserveQL, NDJSON results), `/health`, `/metrics`                            |
| ml-anomaly-detector  | 7600 | `POST /v1/observations`, `/health`, `/metrics`                                                 |
| **alert-manager**    | 7700 | `POST /v1/events`, `POST /v1/observations` (SLO), `POST /v1/slos`, `POST /v1/silences`, `GET /v1/alerts`, `/health`, `/metrics` |
| **cold-tier-controller** | 7800 | `/metrics` (`observex_clickhouse_parts{table,disk}`, `observex_clickhouse_bytes{table,disk}`) |
| **ui-server**        | 8080 | `/` SPA, `/api/tenant/*`, `/api/query/*`, `/api/alert/*` reverse proxies, `/healthz`, `/metrics`, `/config` |
| pprof (gated)        | 4318 | `/debug/pprof/*` when `OBSERVE_X_PPROF_ENABLED=true`                                           |

### Full stack via Docker Compose

```bash
docker compose -f deploy/compose/docker-compose.yml up --build -d
# Prometheus at http://localhost:9090
# Grafana at    http://localhost:3000  (anon viewer, admin/observex)
```

This brings up every ObserveX service plus ClickHouse, Postgres,
Redis, NATS, Prometheus and Grafana. Grafana is pre-provisioned with
the "ObserveX — Platform Overview" dashboard fed from Prometheus.

### Kubernetes (Helm)

```bash
helm install observex deploy/helm/observex \
  --namespace observex --create-namespace \
  --set existingSecret=observex-config
```

The chart is intentionally infra-agnostic — point it at your existing
managed Postgres / ClickHouse / Redis via the values in
`deploy/helm/observex/values.yaml`. Secrets (Postgres DSN, admin
tokens, Slack/PagerDuty keys) come from an existing
`observex-config` `Secret` in the same namespace.

### Operator console

The Phase C-4 UI is served by `services/ui-server` at port 8080.
After `docker compose up`, open http://localhost:8080 — three tabs
(Tenants / ObserveQL / Alerts), authenticated via a bearer token
the operator pastes (in dev mode any token works; production routes
through the same OIDC validator as tenant-api). See
[ADR-0017](./docs/adr/0017-ui-server.md).

To enable OIDC end-to-end in Helm:

```bash
helm upgrade observex deploy/helm/observex \
  --set tenantApi.oidc.enabled=true \
  --set tenantApi.oidc.issuer=https://login.example.com \
  --set tenantApi.oidc.audience=observex \
  --set tenantApi.oidc.adminGroups='{sre,observex-admin}' \
  --set uiServer.oidc.enabled=true \
  --set uiServer.oidc.issuer=https://login.example.com
```

When `tenantApi.oidc.enabled=true`, the static admin-token Secret
key is NOT consumed. tenant-api fails closed if both are set.

### Visualizing metrics / logs / traces (Phase E-0)

The operator console is a control-plane UI; for **chart-style** views
of the tenant data plane (metrics curves, log live-tail, trace
waterfalls) we provision Grafana with a ClickHouse datasource and
three starter dashboards. See
[ADR-0028](./docs/adr/0028-visualization-strategy.md).

After `docker compose -f deploy/compose/docker-compose.yml up -d`,
open http://localhost:3000 (login `admin` / `observex`). The
sidebar shows two folders:

| Folder | Dashboards | Backed by |
|---|---|---|
| `ObserveX / Platform` | `Platform Overview` (signals received, dropped, queue depth, WAL p95, anomalies, HTTP rate, RSS) | Prometheus (self-observability) |
| `ObserveX / Tenant` | `Tenant — Metrics Explorer`, `Tenant — Logs Explorer`, `Tenant — Traces Explorer` | ClickHouse (tenant data) |

Each tenant dashboard exposes a `tenant` template variable
(populated from `SELECT DISTINCT tenant_id`), plus service /
severity / metric filters appropriate to the signal type.

In Kubernetes the chart ships the dashboards and the ClickHouse
datasource as three ConfigMaps (`observex-grafana-datasources`,
`observex-grafana-provider`, `observex-grafana-dashboards`) that you
mount into whichever Grafana you already operate. Disable the
provisioning ConfigMaps entirely with
`--set grafana.provisioning.create=false`.

A native Metrics / Logs / Traces workbench inside `services/ui-server`
is planned as Phase E-1..E-4 ([roadmap.md](./roadmap.md)); Grafana
remains as the SQL escape hatch even after that ships.

### Cold tier (S3)

Mount the canonical storage policy as a ClickHouse ConfigMap:

```bash
helm upgrade observex deploy/helm/observex \
  --set coldTier.enabled=true \
  --set coldTier.storagePolicyConfigMap.create=true
# Then mount the resulting "observex-clickhouse-storage-policies"
# ConfigMap at /etc/clickhouse-server/config.d/ on your CH pods.
```

The cold-tier-controller (`services/cold-tier/cmd`) reports
per-table, per-disk part and byte counts as Prometheus gauges so
operators can alarm on a stalled S3 lifecycle. See
[ADR-0015](./docs/adr/0015-cold-tier.md).

### Querying

```bash
ADMIN="Authorization: Bearer ${OBSERVE_X_TENANT_API_ADMIN_TOKEN}"
KEY=$(curl -s -X POST http://localhost:7400/v1/tenants/acme/api-keys \
  -H "$ADMIN" | jq -r .raw_key)

curl -s -X POST http://localhost:7500/v1/query \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"query":"SELECT severity, body FROM logs WHERE severity = \"ERROR\" SINCE 1h LIMIT 100"}'
```

Response shape (`application/x-ndjson`):

```jsonl
{"_kind":"header","source":"logs","columns":["severity","body"],"limit":100,"estimate":"scan: logs since 1h0m0s, limit 100"}
{"severity":"ERROR","body":"timeout calling /pay"}
{"severity":"ERROR","body":"db connection refused"}
{"_kind":"trailer","rows_returned":2,"duration_ms":17}
```

### Sending real OpenTelemetry data

Any OTel SDK pointed at `http://localhost:4318` with
`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer ${KEY}"` "just works"
against `/v1/traces`, `/v1/metrics`, `/v1/logs`.

### Verify

```bash
go vet ./...
go test -race -count=1 -timeout=180s ./...
go test -bench=BenchmarkEstimateThroughput -benchmem ./tests/benchmarks/
```

## Architecture

```
services/
  ingest-gateway/              receivers (HTTP, gRPC, StatsD) + real OTLP/HTTP
  tenant-api/                  control-plane HTTP API + embedded migrations
  query-engine/                ObserveQL → ClickHouse, NDJSON streaming
  ml-anomaly-detector/         rolling-z anomaly stream

pkg/
  auth/                        KeyStore (Postgres/Memory/Stateless) + Argon2id + mTLS + middleware
  engine/                      ingest pipeline + worker pool
  wal/                         durable mmap WAL with group commit
  storage/clickhouse/          batched, circuit-broken CH backend + query
  actor/  supervisor/          per-tenant TenantActor + OTP-flavoured restart + quarantine
  sampling/                    EWMA-baseline adaptive sampler + optional Redis state
  cep/                         sliding-window complex-event rules (HighErrorRate, HighLatency)
  observeql/                   parser (participle) + planner (tenant-safe, allow-listed)
  plugin/                      wazero WASM host + ABI primitives
  observability/               Prometheus collectors + pprof handlers
  signal/                      canonical Signal type

docs/adr/                      ADRs 0001–0008
tests/{integration,e2e,benchmarks}
```

Data flow:

```
Receiver → ProcessSignal (non-blocking, returns 429 on saturation)
        → bounded ingestCh (default 65536)
        → Decode → Validate → Enrich
        → worker pool (GOMAXPROCS)
        → sampling decision (single owner: engine)
        → WAL.Write (durable within 5ms via group commit)
        → Backend.Write (async batched, circuit broken)
        → actor.Mailbox (CEP, event emission)
```

See [ADR-0001](./docs/adr/0001-base-architecture.md) for the
component-ownership matrix and [ADR-0002](./docs/adr/0002-wal-durability-model.md)
for the WAL contract.

## Observability

Prometheus scrape endpoint: `GET /metrics`.

Phase A series shipped:

| Metric                                            | Type      | Labels                  |
|---------------------------------------------------|-----------|-------------------------|
| `observex_ingest_signals_received_total`          | Counter   | tenant, type            |
| `observex_ingest_signals_dropped_total`           | Counter   | tenant, type, reason    |
| `observex_ingest_pipeline_queue_depth`            | Gauge     | —                       |
| `observex_wal_write_seconds`                      | Histogram | —                       |
| `observex_clickhouse_inflight_batches`            | Gauge     | —                       |

`reason` is one of `overload`, `no_tenant`, `decode`, `actor_full`,
`sampled_out`, `wal_error`.

## Configuration

### ingest-gateway

| Variable                          | Default          | Notes                                  |
|-----------------------------------|------------------|----------------------------------------|
| `OBSERVE_X_POSTGRES_URL`          | (empty)          | **Production**: enables PostgresKeyStore |
| `OBSERVE_X_API_SECRET`            | (empty)          | **Dev only**: enables StatelessKeyValidator; ignored if POSTGRES_URL is set |
| `OBSERVE_X_HTTP_ADDR`             | `:4318`          | HTTP listener                          |
| `OBSERVE_X_GRPC_ADDR`             | `:4317`          | gRPC listener                          |
| `OBSERVE_X_STATSD_ADDR`           | `:8125`          | UDP listener                           |
| `OBSERVE_X_DEFAULT_TENANT`        | `default`        | StatsD tenant fallback (unauthenticated) |
| `OBSERVE_X_WAL_PATH`              | `/tmp/observex/wal` | WAL segment directory                 |
| `OBSERVE_X_SAMPLING_RATE`         | `0.1`            | Adaptive sampler head rate             |
| `OBSERVE_X_TRACE_QUEUE`           | `1000`           | Max in-memory trace candidates         |
| `OBSERVE_X_CLICKHOUSE_ADDR`       | `localhost:9000` | Native ClickHouse address              |
| `OBSERVE_X_CLICKHOUSE_DB`         | `observex`       | Database name                          |
| `OBSERVE_X_CLICKHOUSE_USER`       | (empty)          | Username                               |
| `OBSERVE_X_CLICKHOUSE_PASSWORD`   | (empty)          | Password                               |
| `OBSERVE_X_TLS_CERT_FILE`         | (empty)          | Server cert (enables TLS)              |
| `OBSERVE_X_TLS_KEY_FILE`          | (empty)          | Server key                             |
| `OBSERVE_X_TLS_CA_FILE`           | (empty)          | Client CA (enables mTLS)               |
| `OBSERVE_X_PPROF_ENABLED`         | `false`          | Mount `/debug/pprof/*`                 |

### tenant-api

| Variable                                 | Default | Notes                                |
|------------------------------------------|---------|--------------------------------------|
| `OBSERVE_X_POSTGRES_URL`                 | (required) | pgx DSN                            |
| `OBSERVE_X_TENANT_API_ADMIN_TOKEN`       | (required) | Bootstrap admin auth                |
| `OBSERVE_X_TENANT_API_ADDR`              | `:7400`    | Bind address                        |

## Roadmap snapshot

- ✅ Phase A — stabilise foundation
- ✅ Phase B — all five sub-phases shipped:
  - ✅ **B-1** — tenant control plane, PostgresKeyStore, RLS, audit log
  - ✅ **B-2** — real OTLP protobuf wire format (`/v1/{traces,metrics,logs}`, gzip)
  - ✅ **B-3** — ObserveQL parser + planner + query-engine + NDJSON streaming
  - ✅ **B-4** — stream-processor v2 (supervisor with quarantine, sliding-window CEP, EWMA sampler + optional Redis state)
  - ✅ **B-5** — wazero WASM plugin host + rolling-z anomaly-detector skeleton
- 🔜 Phase C — alert manager, UI, Helm/ArgoCD, DR, security/compliance pass, Arrow IPC results, federated S3 cold tier, ANTLR-driven ObserveQL extensions

Full plan in [`roadmap.md`](./roadmap.md).

## License

Proprietary — all rights reserved.
