# ADR-0025 — Parquet cold-tier export

- Status: Accepted
- Date: 2026-06-01
- Phase: D-8

## Context

ADR-0015 ships ClickHouse cold storage on S3 — long-retention
data lands in `cold_s3` after the configured `MOVE TO DISK`
window. Operators can read it from ClickHouse, but every analytics
tool a customer already runs (Athena, Trino, Spark, DuckDB,
Snowflake's external tables) wants Parquet.

We want operators to be able to export an ObserveQL result set
as Parquet without spinning up a pipeline. The shortest path:
let `query-engine` write Parquet on demand.

## Decision

Add `pkg/parquetexport`:

- `Write(ctx, RowSource, io.Writer, Options)` streams rows into
  Parquet (Snappy compression, 64K row groups).
- `RowSource` is a `Next(ctx) (map, error)` iterator so callers
  can stream from ClickHouse without buffering the full result.
- Schema is inferred from the first row (same type mapping as the
  Arrow IPC codec in ADR-0023).

Add `POST /v1/export` on `query-engine`:

- Accepts the same body as `/v1/query`.
- Streams Parquet to the response with
  `Content-Type: application/x-parquet` and a sensible filename
  in `Content-Disposition`.
- Same `ScopeQuery` requirement as `/v1/query`.

## Trade-offs

- **Materialised export, not push-down to S3** — the executor
  runs the query against ClickHouse and writes Parquet inline.
  Customers wanting "execute on the warehouse, deliver Parquet
  to my bucket" can pipe `aws s3 cp - s3://…` against the
  response body; a managed sink with retries is future work.
- **Snappy default, not Zstd** — Zstd is ~30% smaller but a
  surprising number of analytics tools default to Snappy-only.
  Operators can override via the package; the HTTP endpoint
  takes the default.
- **In-memory row buffer per row group** — keeps memory bounded
  at `RowGroupSize × rowSize` plus the running Parquet writer
  state. At 64K rows × ~512B each that's 32MB; well under any
  reasonable cap.

## Package changes

- `pkg/parquetexport` — new package + tests.
- `services/query-engine/cmd/main.go` — `/v1/export` handler,
  `sliceRowSource` adapter, `executor.QueryRows` helper.
- `services/query-engine/internal/executor/executor.go` — new
  `QueryRows` method.

## Verification

- `go test -race ./pkg/parquetexport/...` — round-trips through
  the Arrow Parquet reader, checks schema + row count.
- Manual: `curl -X POST -d '{"query":"SELECT * FROM logs SINCE 1h"}' \
  http://query-engine:8082/v1/export > /tmp/x.parquet` then
  `duckdb -c "SELECT count(*) FROM '/tmp/x.parquet'"`.
