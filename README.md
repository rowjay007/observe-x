# observe-x

Distributed observability and APM platform written in Go. Self-hosted,
multi-tenant ingestion, processing, storage, and (eventually) query
engine for metrics, logs, traces, and profiling data.

> **Status — Phase A complete.** The Phase 1 foundation has been
> hardened: durable WAL with crash recovery, real ClickHouse driver
> with circuit breaker, single-source pipeline, Prometheus
> self-observability, race-clean tests. See
> [`docs/adr/`](./docs/adr) for the architecture decisions and
> [`roadmap.md`](./roadmap.md) for the remaining phases.

## Quick start

### Prerequisites

- Go **1.24+** (bumped from 1.23 in Phase A because
  `clickhouse-go/v2` requires it — see ADR-0001).
- Docker + Docker Compose for local ClickHouse, Postgres, Redis, NATS.

### Run locally

```bash
docker compose -f tests/docker-compose.yml up -d

export OBSERVE_X_API_SECRET="dev-only-secret"            # see ADR-0003
export OBSERVE_X_WAL_PATH=/tmp/observex/wal
export OBSERVE_X_CLICKHOUSE_ADDR=localhost:9000
export OBSERVE_X_CLICKHOUSE_DB=observex

go run ./services/ingest-gateway/cmd
```

The gateway exposes:

| Endpoint           | Port | Notes                                       |
|--------------------|------|---------------------------------------------|
| HTTP (Gin)         | 4318 | `/v1/ingest`, `/v1/otlp/traces`, `/health`, `/ready`, `/metrics` |
| gRPC               | 4317 | OTLP-shaped `IngestService.Export`          |
| StatsD UDP         | 8125 | DogStatsD-compatible wire format            |
| pprof (gated)      | 4318 | Mounted at `/debug/pprof/*` when `OBSERVE_X_PPROF_ENABLED=true` |

Per-tenant API keys for testing are derived from
`OBSERVE_X_API_SECRET` (dev-only — see security note below):

```bash
# Generate a dev key for tenant "demo"
go run ./services/ingest-gateway/cmd/devtools/keygen demo  # planned for Phase B

# Phase A workaround: derive manually
TENANT=demo
HASH=$(printf "%s:%s" "$OBSERVE_X_API_SECRET" "$TENANT" \
       | b3sum --raw | xxd -p -c 256)
KEY="${TENANT}:${HASH}"

curl -X POST http://localhost:4318/v1/ingest \
  -H "Authorization: Bearer ${KEY}" \
  -H "Content-Type: application/json" \
  -d "{\"tenant_id\":\"${TENANT}\",\"type\":\"METRIC\",
       \"payload\":\"eyJ2YWx1ZSI6MX0=\",
       \"attributes\":{\"metric_name\":\"requests\"}}"
```

> **Security note.** The single-secret `StatelessKeyValidator` is
> **dev-only**. Any leak of `OBSERVE_X_API_SECRET` lets an attacker
> mint a valid key for every tenant. Production deployments MUST wait
> for the Phase B `PostgresKeyStore` (per-tenant Argon2id keys with
> revocation). See [ADR-0003](./docs/adr/0003-auth-and-tenant-isolation.md).

### Verify

```bash
go vet ./...
go test -race -count=1 -timeout=180s ./...
go test -bench=BenchmarkEstimateThroughput -benchmem ./tests/benchmarks/
```

## Architecture

```
services/ingest-gateway/       receivers (HTTP, gRPC, StatsD) + auth
pkg/engine/                    pipeline composition + worker pool
pkg/wal/                       durable mmap WAL with group commit
pkg/storage/clickhouse/        batched, circuit-broken CH backend
pkg/actor/  pkg/supervisor/    per-tenant TenantActor + restart
pkg/sampling/                  adaptive head sampler
pkg/cep/                       complex-event-processing rules
pkg/observability/             Prometheus collectors + pprof handlers
pkg/signal/                    canonical Signal type
docs/adr/                      architecture decision records
proto/                         protobuf source (OTLP stubs in Phase B)
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

| Variable                          | Default          | Notes                                  |
|-----------------------------------|------------------|----------------------------------------|
| `OBSERVE_X_API_SECRET`            | (required)       | Dev-only shared secret                 |
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

## Roadmap snapshot

- ✅ Phase A — stabilise foundation (this release)
- 🔜 Phase B — tenant control plane, real OTLP wire format,
  query engine + ObserveQL, federated execution
- 🔜 Phase C — anomaly detection, alert manager, UI, Helm/ArgoCD,
  DR, security/compliance pass

Full plan in [`roadmap.md`](./roadmap.md).

## License

Proprietary — all rights reserved.
