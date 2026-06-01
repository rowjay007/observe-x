# ADR-0027 — Federated S3 + DuckDB executor seam

- Status: Accepted
- Date: 2026-06-01
- Phase: D-10

## Context

ClickHouse is our hot store; S3 + Parquet (ADRs 0015, 0025) is
the long-retention store. A real ObserveQL query — "show me the
last 90 days of error logs for tenant X" — straddles both.
Today the planner only knows about ClickHouse, so cold-tier
queries either re-import data from S3 into ClickHouse first
(expensive) or are unavailable.

We need a federated executor: one logical query, two physical
backends, results merged. DuckDB is the standard answer for
in-process Parquet-over-S3 — it has `httpfs`, predicate pushdown,
and a column-oriented vectorised executor — but it requires CGO
which we don't want as a hard dependency.

## Decision

Add `pkg/federation`:

- `Backend` interface — `Name`, `Execute(ctx, sql, params)`.
- `Router` — binds source labels to backends, routes
  `Execute(source, sql, params)` calls.
- `Router.ExecuteUnion(sources, sql, params)` — fans the same SQL
  across multiple backends in parallel, fail-fast on error,
  merges into a single result, stable-sorts by `_ts` /
  `timestamp` / `start_time` / `ts` so a hot+cold union reads
  chronologically.
- `NewDuckDBBackend` — build-tag gated:
  - `duckdb_stub.go` (default): returns `ErrUnsupported`.
  - `duckdb_runtime.go` (`-tags duckdb`): opens an in-process
    DuckDB, installs `httpfs`, configures S3 region/endpoint,
    and forwards SQL.

## Trade-offs

- **DuckDB behind a build tag** — same pattern as the ONNX
  runtime in ADR-0016. Operators who don't need federation get
  a binary with no CGO dep; operators who do build with
  `-tags duckdb` (and accept the DuckDB shared-lib dep).
- **Per-backend SQL, not a unified rewriter** — `ExecuteUnion`
  takes a single SQL string and runs it as-is on every backend.
  Real federation needs per-backend SQL rewrites (table name
  differences, function dialect). That's the next ADR; for
  v1.0 we ship the routing seam.
- **Best-effort sort merge** — sort key inferred from common
  column names. Will be replaced with the CBO's chosen merge
  order once the optimiser knows about federation.
- **Synchronous result aggregation** — `ExecuteUnion` waits for
  all backends before returning; a streaming pipeline would
  require touching every backend's executor interface.

## Package changes

- `pkg/federation` — new package + tests.
- `pkg/federation/duckdb_stub.go` — default build.
- `pkg/federation/duckdb_runtime.go` — `+build duckdb`.

## Configuration

DuckDB backend (when enabled):

- `OBSERVE_X_DUCKDB_S3_REGION=us-east-1` — required.
- `OBSERVE_X_DUCKDB_S3_ENDPOINT=…` — optional (MinIO / R2).
- `OBSERVE_X_DUCKDB_MEMORY_MB=4096` — optional.
- `OBSERVE_X_DUCKDB_THREADS=8` — optional.

## Verification

- `go test -race ./pkg/federation/...` — covers routing,
  fan-out merge ordering, fail-fast, and the `ErrUnsupported`
  contract of the stub.
- Manual (with `-tags duckdb`): query `SELECT … FROM
  read_parquet('s3://observex-cold/clickhouse/logs/*.parquet')`
  through the router; confirm results match a parallel
  ClickHouse query for overlap rows.
