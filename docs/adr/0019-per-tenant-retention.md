# ADR-0019 — Per-tenant cold-tier retention

- Status: Accepted
- Date: 2026-06-01
- Phase: D-2

## Context

ADR-0015 introduced the `hot_cold` storage policy with a single
TTL window per table: metrics 30 → 90 d, logs 14 → 30 d,
traces 7 → 30 d. That's a sensible default but a fixed default —
some customers have 90-day compliance retention on logs, others
want metrics on hot storage for a year for trend analysis. A
single global TTL forces us to over-provision storage for the
entire cluster to satisfy the strictest tenant.

## Decision

Add a `pkg/retention` package that takes a per-tenant `Spec` and
emits `ALTER TABLE … MODIFY TTL … WHERE tenant_id = '<id>'`
statements. ClickHouse evaluates the TTL row-by-row, so a single
physical table holds a mix of tenant-specific lifecycles.

Expose two new admin endpoints in `tenant-api`:

- `PUT /v1/tenants/:id/retention` — set the override.
- `DELETE /v1/tenants/:id/retention` — revert to defaults.

Both require an authenticated operator (OIDC if configured, static
admin token otherwise) and emit audit records via the existing
`audit()` helper.

## Validation

- Tenant ID is re-validated at the DDL boundary against
  `[a-z0-9_-]{1,63}` — defence in depth against SQL injection
  since TTL clauses can't take parameters.
- Hot days must be ≤ total days.
- Per-table caps (metrics 10y, logs/traces 2y) prevent
  pathological values.

## Trade-offs

- **DDL per tenant, not a control table** — keeping the policy in
  ClickHouse metadata rather than a side table means the engine
  enforces it natively (no per-query rewrite, no application-side
  cron). The downside is metadata bloat at very high tenant
  counts; the ADR-0015 `system.parts` gauges already monitor it.
- **No retroactive enforcement** — `MOVE TO DISK` and `DELETE`
  TTL rules apply at merge time. A tenant going from 30 → 7 days
  hot won't see data move to S3 for several hours as background
  merges process the affected parts. Acceptable for the use case.
- **No grandfathering on lowered retention** — if a tenant
  shortens their total window, older data is eligible for
  deletion on the next merge. Operators must communicate
  shortenings explicitly.

## Package changes

- `pkg/retention` — new package + tests.
- `services/tenant-api/cmd/main.go` — new endpoints, optional
  ClickHouse connection, audit hooks. Returns 501 if
  `OBSERVE_X_CLICKHOUSE_ADDR` is not configured on tenant-api.

## Configuration

- `OBSERVE_X_CLICKHOUSE_ADDR=clickhouse:9000` — required on
  tenant-api to enable the retention API.

## Verification

- `go test -race ./pkg/retention/...`
- Manual: `curl -X PUT … /v1/tenants/acme/retention` then
  `SELECT engine_full FROM system.tables WHERE name = 'metrics'`
  to confirm the new TTL is registered.
