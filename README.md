# observe-x

Distributed observability and APM platform written in Go. Self-hosted,
multi-tenant ingestion, processing, storage, and query engine for
metrics, logs, traces, and profiling data.

> **Status — Phase B complete.** All five Phase B sub-phases have
> landed. ObserveX now has real OTLP/HTTP receivers, a Postgres-
> backed tenant control plane with RLS, an OTP-flavoured supervisor
> with restart-strategy and quarantine, a sliding-window CEP engine,
> an EWMA-baseline adaptive sampler with optional Redis state, an
> ObserveQL parser + query engine that streams NDJSON results from
> ClickHouse with tenant-id injection, a wazero-based WASM plugin
> host, and a rolling-z-score anomaly-detector skeleton.
> See [`docs/adr/`](./docs/adr) for the eight ADRs and
> [`roadmap.md`](./roadmap.md) for the Phase C scope.

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
KEY=$(curl -s -X POST http://localhost:7400/v1/tenants/acme/api-keys \
  -H "$ADMIN" | jq -r .raw_key)

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
| pprof (gated)        | 4318 | `/debug/pprof/*` when `OBSERVE_X_PPROF_ENABLED=true`                                           |

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
