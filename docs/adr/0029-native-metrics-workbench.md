# ADR-0029 — Native Metrics workbench in `ui-server` (Phase E-1)

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-1
* **Related**: ADR-0017 (ui-server), ADR-0023 (Arrow IPC), ADR-0028 (visualization strategy)

## Context

Phase E-0 (ADR-0028) gave operators a working visual stack via
provisioned Grafana dashboards. That ships the data, but the SRE
"three-pane" workflow still happens *outside* the ObserveX brand,
and every alert→trace→log linking dies at the Grafana boundary.

This ADR ships the first native panel type — **time-series metrics
charts inside `services/ui-server`** — laying the foundation the
Logs (E-2), Traces (E-3), and Dashboards (E-4) tabs will reuse.

## Decision

Add a `Metrics` tab to the SPA with:

* A canvas-based time-series chart primitive (`assets/chart.js`,
  ~300 LOC, zero dependencies). Rendered on `<canvas>` with HiDPI
  awareness, axis auto-formatting, tooltip + crosshair on hover,
  click-drag time-range select, and configurable fill/log Y.
* A multi-panel grid (CSS Grid `auto-fit minmax(420px,1fr)`).
  Each panel owns: title, ObserveQL textarea, chart, error slot.
* Tenant input + time-range select (15m / 1h / 6h / 24h / 7d) +
  refresh interval (off / 10s / 30s / 1m), all global to the tab.
* "+ Panel" button to append; "✕" on a panel head to remove.
* Each panel POSTs its query to the existing
  `/api/query/v1/query` endpoint and parses the NDJSON response
  shape we shipped in Phase B-3, converting rows into chart
  series via a small heuristic (first time column → X, numeric
  columns → Y, string column → series-bucketing).

The chart primitive is intentionally *not* an external library:
no uPlot, no ECharts, no Chart.js. The ui-server already commits
to a zero-build, embedded-asset philosophy; vendoring a 40 KB
minified blob plus a license header fights that ethos. A 300-LOC
hand-rolled canvas renderer covers everything we need (line/area
series, time X axis, log/lin Y, tooltip, crosshair, range select)
and lets us style for the dark theme without `!important`
overrides.

## Why NDJSON, not Arrow IPC

We ship Arrow IPC in `query-engine` (ADR-0023) — but parsing
Arrow in the browser requires the `apache-arrow` JS bundle
(~250 KB), again against zero-build. For typical metric panels
(seconds × tens of series × hundreds of points = O(10k) rows),
NDJSON parsing in the main thread is sub-10ms. The break-even
where Arrow pays off is ~100K rows per panel; we'll revisit when
that becomes a real workload.

## Trade-offs

| ✓ | ✗ |
|---|---|
| Single-binary deliverable, no build step, no npm. | We own a charting renderer forever (the 300 LOC). |
| Tight visual integration — the chart looks like the rest of the operator console, not "Grafana with a different font". | We don't get free PromQL templating, alerting-from-panel, plugins — those weren't part of the ObserveX promise anyway. |
| ObserveQL stays the only query language users see. | NDJSON has slightly more parser overhead per byte than Arrow at scale; we accept this until the workload demands otherwise. |
| Tooltip/crosshair, range-select, HiDPI, log Y, fill — all there in <300 LOC. | Per-series threshold lines / annotations are not in v1 of the renderer; queued for E-4. |

## What ships

| File | Purpose |
|---|---|
| `services/ui-server/cmd/assets/chart.js` | `ObservexChart` class — canvas rendering, axes, tooltip, range-select. |
| `services/ui-server/cmd/assets/index.html` | New `#panel-metrics` section with controls and panel grid. |
| `services/ui-server/cmd/assets/app.js` | Panel CRUD, NDJSON→series adapter, time-range / refresh wiring. |
| `services/ui-server/cmd/assets/app.css` | `.panel-grid`, `.metric-panel`, canvas sizing. |

The Logs (E-2), Traces (E-3), and Dashboards (E-4) tabs are
scaffolded in the same commit so their UI shells are visible
behind feature work — the JS modules for each are also in place
and start working as soon as their backends land in their
respective ADRs.

## Acceptance criteria

* `go build ./services/ui-server/cmd` succeeds.
* `curl /` returns the SPA with all four new tab buttons.
* `curl /chart.js` returns the chart primitive (200 OK).
* Clicking "+ Panel" with `metrics-tenant=acme` and the default
  query against a populated ClickHouse renders a non-empty time
  series within 2 seconds.
* CI: `go vet`, `go test -race ./...`, `helm template` all clean.
