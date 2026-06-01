# ADR-0028 — Visualization strategy: Grafana now, native workbench in Phase E

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-0 (visualization bootstrap)
* **Related**: ADR-0017 (operator UI), ADR-0023 (Arrow IPC codec), ADR-0025 (Parquet export), ADR-0017 (UI server)

## Context

By the close of v1.0 (Phase D), ObserveX ingests, stores, queries, alerts on,
and exports metrics / logs / traces, but the only first-party UI we ship is the
**operator console** at `services/ui-server` — Tenants, Query (NDJSON dump),
Alerts, Audit. There is no charting, no log-line virtualizer, no trace
waterfall. An SRE looking at "p99 latency for `checkout-api` over the last
hour" today has to:

1. open the operator UI Query tab
2. paste an ObserveQL query
3. read 500 lines of NDJSON in a `<pre>` block.

That is not a product. It is a debug endpoint. We need real visual
surfaces for the three signal types — but building Grafana-parity inside
`ui-server` is realistically 8–12 weeks of work and we want users seeing
their data **this sprint**.

## Decision

Adopt a **hybrid two-phase strategy**:

1. **Phase E-0 (this ADR, shipping now)** — provision Grafana with a
   ClickHouse datasource and three tenant-facing dashboards
   (`tenant-metrics`, `tenant-logs`, `tenant-traces`). The Grafana
   instance lives alongside the existing Prometheus / Grafana stack we
   already deploy for self-observability. Operators get a recognisable,
   battle-tested visualization surface immediately, with no new code in
   the hot path.

2. **Phase E-1..E-4 (planned)** — build a native visualization
   workbench inside `services/ui-server`: Metrics charts (uPlot or
   ECharts on top of the Arrow IPC codec we already added in ADR-0023),
   Logs explorer (virtualized table + live tail over the existing SSE
   fan-out from ADR-0020), Trace waterfall + service map, and
   dashboards persistence. The Grafana stack remains as a
   power-user / SQL escape hatch even after the native workbench
   lands.

## What we shipped in E-0

| Artifact | Path | What it does |
|---|---|---|
| ClickHouse datasource | `deploy/grafana/provisioning/datasources/clickhouse.yml` | Provisioned read-only datasource pointing at the `observex` database, with logs/traces column hints so Grafana's Logs and Traces panels render natively. |
| Tenant-Metrics dashboard | `deploy/grafana/dashboards/tenant/tenant-metrics.json` | Time-series of `metrics.value` grouped by `metric_name` / `service`, top-cardinality bar gauge, ingest-rate panel, latest-samples table. Tenant + service + metric template variables. |
| Tenant-Logs dashboard | `deploy/grafana/dashboards/tenant/tenant-logs.json` | Log-volume time-series, native Logs panel reading `body` / `severity` / `trace_id`, full-text body filter, severity multi-select. |
| Tenant-Traces dashboard | `deploy/grafana/dashboards/tenant/tenant-traces.json` | Span-rate, p50/p95/p99 latency, error-rate stat, top slow operations bargauge, recent-traces table with click-through to a waterfall in Grafana's Explore view. |
| Provisioning split | `deploy/grafana/provisioning/dashboards/dashboards.yml` | Two providers — `ObserveX / Platform` (self-observability) and `ObserveX / Tenant` (data plane) — so the Grafana sidebar separates the two audiences. |
| Compose wiring | `deploy/compose/docker-compose.yml` | `GF_INSTALL_PLUGINS=grafana-clickhouse-datasource` on the Grafana service; depends on ClickHouse being healthy. |
| Helm provisioning | `deploy/helm/observex/templates/grafana-provisioning.yaml` | Three ConfigMaps the operator mounts into their existing Grafana deployment: datasources, dashboards provider config, and the dashboard JSONs themselves (sourced via `.Files.Glob`). |

## Trade-offs considered

### Path A — Grafana only (rejected as the *only* answer)

| ✓ | ✗ |
|---|---|
| Zero frontend code. Industry-default UX. | Operators see two UIs (admin console + Grafana) with two auth domains unless we proxy. |
| Free alerting, templating, sharing, RBAC. | Grafana queries our tables in raw ClickHouse SQL, not ObserveQL — language semantics drift. |
| Ships in days, not months. | Branding/UX is Grafana's. Long-term we lose product cohesion. |

### Path B — Native workbench only (rejected as the *first* answer)

| ✓ | ✗ |
|---|---|
| Single product, single brand, single OIDC session. | 8–12 weeks before anyone sees a chart. Customers blind that whole time. |
| ObserveQL is the only query language. | We become responsible for 10 years' worth of edge cases Grafana already fixed. |
| Full control over UX: alert→trace→log linking is first-class. | Larger maintenance surface; we own a charting lib forever. |

### Path C — Hybrid (chosen)

| ✓ | ✗ |
|---|---|
| Visual stack today (Grafana) + native plan for the right long-term UX. | Grafana provisioning is one more thing to keep current. |
| Both paths reinforce each other — Grafana validates the ClickHouse schema; the native workbench reuses Arrow IPC + SSE we already built. | Until E-1..E-4 ships, two UIs co-exist. |
| Customers can immediately do the high-value SRE workflow (chart a metric, tail logs, find slow traces) with zero code by us. | We have to keep dashboard JSON honest — `helm template` exercises this in CI. |

## Why not Loki / Tempo / Mimir?

Those are Grafana Labs' first-party storage backends. We already have a
storage backend — ClickHouse — that is faster, cheaper, and unified
across all three signal types. Adopting Loki/Tempo/Mimir would duplicate
storage, double our operational surface, and undo the whole point of the
unified-store ADRs (0001, 0002, 0007). The
`grafana-clickhouse-datasource` plugin lets us get Grafana's *frontend*
benefits without taking on its *backend* dependencies.

## Why not embed Grafana inside `ui-server` via iframe?

Considered and rejected. iframe-embed has known pitfalls around CSP,
cookie sharing, deep linking, and resize behaviour; debugging them eats
weeks. The cleaner separation is: one OIDC session, two URLs, both
behind the same ingress. When E-1..E-4 ship, the operator UI's
Metrics/Logs/Traces tabs link out to the equivalent Grafana view as a
fallback — not embed it.

## Migration plan

| When | What |
|---|---|
| Now (this ADR) | Grafana + 3 tenant dashboards live; documented in README. |
| E-1 (next sprint) | Native Metrics tab in `ui-server` using uPlot + Arrow IPC. Grafana metrics dashboard remains; UI tab is preferred for tenant-facing customers. |
| E-2 | Native Logs explorer with virtualized table + SSE live tail. Grafana logs dashboard remains for ad-hoc SQL. |
| E-3 | Native Trace waterfall + service map. Grafana traces dashboard remains for advanced filters. |
| E-4 | Dashboards CRUD inside `ui-server` (Postgres-backed); operator can pin native panels to a layout and share by URL. |
| E-5 (optional) | PromQL / LogQL shim on `query-engine` so Grafana keeps working with zero ClickHouse-SQL leakage. |

## Acceptance criteria

* `docker compose -f deploy/compose/docker-compose.yml up -d` brings up
  Grafana at `http://localhost:3000` with the ClickHouse datasource
  green and all four dashboards visible.
* `helm template observex deploy/helm/observex` renders the three
  Grafana ConfigMaps (CI gate added).
* `tenant-metrics`, `tenant-logs`, `tenant-traces` open in <2 seconds
  against a populated `observex` database.
