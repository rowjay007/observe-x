# ADR-0032 — Dashboards CRUD (Phase E-4)

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-4
* **Related**: ADR-0004 (tenant control plane), ADR-0017 (ui-server), ADR-0029 (metrics workbench)

## Context

E-1 shipped a Metrics tab where every panel is a textarea + a
chart. That's enough for ad-hoc exploration but it leaks the
canonical SRE pattern — operators want **saved layouts** they can
return to, share via URL, and round-trip as JSON.

We need a tenant-scoped CRUD surface for dashboards, opaque enough
that we can evolve the SPA panel schema without redeploying
tenant-api.

## Decision

A new `dashboards` table in the tenant-api Postgres schema, with
five REST endpoints on the existing admin-gated `/v1` group:

| Method | Path | Handler |
|---|---|---|
| GET | `/v1/dashboards?tenant_id=X` | list (newest first, 200 cap) |
| GET | `/v1/dashboards/:id` | single lookup |
| POST | `/v1/dashboards` | create — `{tenant_id, name, layout}` |
| PUT | `/v1/dashboards/:id` | replace name + layout |
| DELETE | `/v1/dashboards/:id` | hard delete |

### Schema (`migrations/002_dashboards.sql`)

```sql
CREATE TABLE dashboards (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    layout     JSONB NOT NULL,                  -- {panels: [...]}
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX dashboards_tenant_name_uq ON dashboards (tenant_id, name);
ALTER TABLE dashboards ENABLE ROW LEVEL SECURITY;
CREATE POLICY dashboards_isolation ON dashboards
    USING (tenant_id = current_setting('app.tenant_id', true));
```

Plus a `BEFORE UPDATE` trigger that touches `updated_at` so the
application layer can't forget on PATCH paths.

### Why layout is `JSONB` and opaque server-side

The SPA owns the panel schema (title, query, plus future per-type
config like axis labels, threshold lines, log-Y toggle). If we
encoded panels as separate rows, every UI evolution would force a
matching Postgres migration. Storing the whole layout as JSONB
lets us iterate on the panel schema without redeploying
tenant-api, while still giving us:

* Postgres-side JSONB indexing if we ever want to query "which
  dashboards reference this metric?".
* Safe re-encode at API boundary (`json.RawMessage` passthrough
  so the JSON encoder embeds layout bytes verbatim, no double-
  encoding).

Server-side validation is intentionally limited:

1. Body must parse as JSON (Gin's `ShouldBindJSON`).
2. `layout` field must decode to a JSON **object** (`{...}`).
   Strings, arrays, primitives are rejected at the boundary so a
   corrupt or attacker-crafted payload can't poison the row or
   break downstream consumers.
3. `(tenant_id, name)` is `UNIQUE` so the SPA's "Open" button is
   unambiguous; duplicate POST → `409 Conflict` with a hint to
   PUT instead.

We do NOT validate the panels schema. The SPA is the source of
truth; tenant-api treats it as opaque bytes.

### Sharing model

* Within a tenant — share-by-URL via `#dash=<uuid>`. The SPA
  recognises the hash on load, fetches the dashboard, and opens
  it in the Metrics tab.
* Cross-tenant — NOT supported in v1 by design. RLS keys on
  `tenant_id` so a query "select from dashboards" returns only
  the connection's pinned tenant. To move a dashboard between
  tenants, the operator exports JSON from tenant A and imports it
  into tenant B via the SPA's Import button. This keeps the
  audit trail clean (who imported what, when, from where) and
  avoids the "shared dashboard mutates beneath you" multi-tenant
  hazard.
* Public URLs — NOT supported in v1. If we add them later they go
  through a separate `shared_dashboards` table with TTL and
  per-link scopes, never by leaking the `dashboards.id` UUID.

## Trade-offs

| ✓ | ✗ |
|---|---|
| Single Postgres table, single migration — zero new infra. | Layout drift between SPA versions is a UX problem (we mitigate via additive JSON schema). |
| RLS belt-and-suspenders: even if app code forgets the tenant filter, Postgres refuses cross-tenant reads. | Operators with admin scope can still see across tenants if they connect with BYPASSRLS — by design, that's the operator persona. |
| Layout opaque server-side ⇒ SPA evolves panel schema without tenant-api releases. | Server can't reason about panel content (e.g. "find all dashboards using deprecated metric X"). Acceptable for v1; if needed, a `jsonb_path_query` indexer ships later. |
| (tenant_id, name) UNIQUE ⇒ Open button is unambiguous. | Operators have to invent unique names within a tenant; PUT-by-id sidesteps the limit. |
| Dashboards inherit tenant lifecycle — `ON DELETE CASCADE`. | A deleted tenant's dashboards are unrecoverable; that's the right default for compliance. |

## Wire shape

```jsonc
// POST /v1/dashboards
{
  "tenant_id": "acme",
  "name":      "checkout-ops",
  "layout": {
    "panels": [
      { "title": "RPS",            "query": "select toStartOfMinute(timestamp) as t, avg(value) as v from metrics where metric_name='rps' group by t order by t" },
      { "title": "p99 latency ms", "query": "select toStartOfMinute(timestamp) as t, quantile(0.99)(value) as v from metrics where metric_name='latency_ms' group by t order by t" }
    ]
  }
}
// → 201
{ "id": "8d4f…", "tenant_id": "acme", "name": "checkout-ops", "layout": {…}, … }
```

## What ships

| File | Purpose |
|---|---|
| `services/tenant-api/store/migrations/002_dashboards.sql` | Table + index + RLS policy + updated_at trigger. |
| `services/tenant-api/store/store.go` | `Dashboard` type, `Err*` sentinels, `List/Get/Create/Update/Delete` methods. |
| `services/tenant-api/cmd/main.go` | Five handlers, `dashboardJSON` serializer, `isJSONObject` validator. |
| `services/tenant-api/cmd/dashboards_test.go` | Roundtrip test for the JSON serializer + 8-case validation matrix. |
| `services/ui-server/cmd/assets/app.js` | Dashboards panel module (CRUD UI + JSON import/export + share-by-URL) — shipped in E-1. |

## Acceptance criteria

* `go build ./services/tenant-api/cmd` clean.
* `go test ./services/tenant-api/cmd` — `dashboardJSON` roundtrip
  preserves layout shape; validation matrix passes for all 8 cases.
* Integration (when Postgres is up): POST → GET → PUT → DELETE
  → 404 round-trip works end-to-end; duplicate POST returns 409.
* RLS check: a connection with `app.tenant_id = 'acme'` sees zero
  rows from a dashboard owned by `widgets-inc`.
* SPA: clicking a dashboard's "Open" loads it into the Metrics tab;
  export downloads a `.json` file the user can edit and re-import.
