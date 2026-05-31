# ADR-0015 — S3 + ClickHouse cold tier

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3b

## Context

Phase A built ObserveX on plain `MergeTree` tables with hard TTL
DELETE windows (metrics 30d, logs 14d, traces 7d). That is fine for
prototypes and for tenants whose retention SLA matches the hard-coded
window, but it has two production problems:

1. **Cost.** Local SSD / EBS is 5–20× the cost of S3 Standard.
   Beyond ~7 days, the data is overwhelmingly read for compliance
   queries, not interactive ones — paying SSD pricing for it is
   wasteful.
2. **Compliance retention.** SOC2 / HIPAA / GDPR-style policies want
   traces and logs retained 30–365 days for audit, even though the
   active query window is much shorter.

ClickHouse already solves both with **multi-disk storage policies**
plus `TTL ... TO DISK ... TO DISK ... DELETE` clauses. We pick that
native solution over writing our own tier-down service that copies
parts to S3, because ClickHouse's implementation handles the hard
parts (cache, S3 multipart, async background moves, query reads from
S3 via the same SELECT path) that we would otherwise have to rebuild
poorly.

## Decision

Ship the cold tier in three pieces:

1. **`deploy/clickhouse/storage_policies.xml`** — operator-mounted
   ClickHouse config that declares a `cold_s3` disk and a `hot_cold`
   policy. Defaults to AWS S3 via the SDK's default credential chain
   (so IRSA / Pod Identity / IMDS just work); the file is annotated
   for operators who need to swap in static credentials.

2. **Migration `002_cold_tier.sql`** — three `ALTER TABLE`
   statements that set `storage_policy = 'hot_cold'` and rewrite the
   TTL clauses to:

   | table   | hot tier | cold tier (S3) | delete |
   |---------|----------|----------------|--------|
   | metrics | 30 days  | +60 days       | 90 days total |
   | logs    | 14 days  | +16 days       | 30 days total |
   | traces  |  7 days  | +23 days       | 30 days total |

   The migration runner tolerates ClickHouse error code 243
   (`NO_AVAILABLE_DISK`) so single-disk dev clusters can still apply
   the schema and degrade gracefully to "hot-only, hard delete on
   the original TTL window."

3. **`services/cold-tier-controller`** — a thin read-only Go service
   that scrapes `system.parts` every 60s, grouped by
   `(table, disk_name)`, and exposes two Prometheus gauges
   (`observex_clickhouse_parts`, `observex_clickhouse_bytes`) so
   operators get drift alarms ("cold-tier S3 moves stalled") without
   having to run ad-hoc `clickhouse-client` from a shell. The
   controller does NOT trigger moves — the ClickHouse background
   merger does that — but its existence is necessary because the
   default Prometheus exporters for ClickHouse don't break down
   parts per disk.

## Trade-offs

- **No per-tenant TTL today.** The migration sets a single retention
  window per table. Per-tenant retention is a `ALTER TABLE … MODIFY
  TTL … WHERE tenant_id = $X` change that the tenant-api will
  expose in a follow-up. For now, operators that need it can run
  the ALTER manually from the tenant-api shell; the documented
  query is in the migration's header comment.

- **Cold reads pay S3 round-trip latency.** A query that selects
  data older than the hot window has 50–250ms additional latency
  from S3 GETs. We accept it because the hot window is set such
  that interactive queries (the P99 SLA path) stay on hot, and
  audit-style queries can tolerate the extra latency.

- **S3 PUT/GET cost.** Background moves are sequential PUTs of
  100MB–5GB parts; not a cost driver. Cold-read GETs are page-sized
  and benefit from ClickHouse's local cache (`data_cache_max_size
  = 10Gi` in the example XML). For very chatty audit consumers,
  bump the cache.

- **Storage-policy XML must be ops-managed.** ClickHouse does not
  accept storage-policy declarations via SQL (it's a server-startup
  concept). The XML must be mounted on every replica and reloaded
  with `SYSTEM RELOAD CONFIG`. The Helm chart adds a ConfigMap + a
  volumeMount for ClickHouse pods to make this declarative.

- **No Parquet export.** A common ask is "also write Parquet files
  to S3 in OTel-conformant schema so the data is queryable from
  Athena / BigQuery / Trino directly." Out of scope for this slice;
  ClickHouse's own `S3` engine and `SELECT INTO OUTFILE FORMAT
  Parquet` cover the use case for now. We ADR-flag it for Phase
  C-4+ if the demand materializes.

## Package changes

- `pkg/storage/clickhouse/migrations/002_cold_tier.sql` (new):
  storage_policy + multi-stage TTL ALTERs.
- `pkg/storage/clickhouse/backend.go` (modified): embeds and runs the
  new migration after the initial schema.
- `pkg/storage/clickhouse/client.go` (modified): `RunMigrations`
  tolerates `NO_AVAILABLE_DISK` (243), `Cannot find storage policy`.
- `deploy/clickhouse/storage_policies.xml` (new): operator-mounted
  multi-disk config.
- `services/cold-tier/cmd/main.go` (new): scraping controller.

No new top-level Go dependencies. ClickHouse driver
(`clickhouse-go/v2`) and Prometheus client are already in the graph.

## Verification

- `go build ./services/cold-tier/...` — clean.
- Existing `go test ./pkg/storage/clickhouse/...` continues to pass
  against single-disk MinIO/ClickHouse in `tests/docker-compose.yml`
  (the cold-tier migration is the no-op path on that fixture).
- Manual e2e (left for the next operator on the next prod-shaped
  cluster): write metrics, wait past the hot TTL, verify
  `system.parts` shows the new disk for older parts.
