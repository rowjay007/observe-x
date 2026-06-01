# ADR-0031 — Native Trace waterfall + service map (Phase E-3)

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-3
* **Related**: ADR-0029 (metrics workbench), ADR-0030 (logs explorer), ADR-0028 (visualization strategy)

## Context

Traces are the third signal type that needs a native UX. The
critical SRE workflow is:

1. See a slow request in the alert / log feed (alert → trace).
2. Open the trace.
3. Read a Gantt-style **waterfall** that shows which spans
   contributed how much wall-clock time.
4. Optionally see a **service map** for the call graph in this
   one trace, to understand which downstream service is the
   culprit.

Grafana's Tempo / Jaeger panels do this for their own backends.
We have all the data in the ClickHouse `traces` table already
(see `migrations/001_initial_schema.sql`); the only missing piece
is the rendering surface in `services/ui-server`.

## Decision

Add a `Traces` tab with three controls — Search (top-left list),
Waterfall (top-right), Service Map (bottom-right) — and two
canvas/DOM renderers, both shipped in `assets/waterfall.js`:

* **renderWaterfall(host, spans)**

  Normalises every span to micro-second offsets from the trace's
  earliest `start_time`. Builds the parent→child index, sorts
  children by start, and emits a depth-first preorder traversal.
  Each row is a flexbox with three slots:

      [ indent-label  ] [ bar in absolute % of trace span ] [ ms label ]

  Bar color: `--accent` for OK, `--err` for non-OK status. Indent
  is 4 nbsp per parent depth — Gantt-style, no scroll-x.

* **renderServiceMap(canvas, spans)**

  Builds a directed multigraph of unique services (nodes) with
  edges = inter-service parent→child calls (weighted by count).
  Lays nodes on a circle (no force-directed solver — for a single
  trace this is rarely > 8 services, and a circular layout is more
  legible than a settled graph). Draws weighted edges with
  arrowheads pointing parent→child; weight maps to stroke width
  (`min(4, 1 + log2(count+1))`). Nodes are 18 px filled circles
  with the service name truncated to 10 chars centred inside.

  HiDPI aware (sets `canvas.width = clientWidth * devicePixelRatio`,
  resets transform per render).

* **Search panel** runs ObserveQL against the `traces` table with
  tenant + service + min-duration filters; click a row and the
  detail pane refetches all spans for that `trace_id` and feeds
  both renderers.

All three views drink from the existing `/v1/query` NDJSON
endpoint — no new server surface needed.

## Why a custom waterfall instead of Jaeger's React component

Jaeger ships [`jaeger-ui`](https://github.com/jaegertracing/jaeger-ui)
which has a polished waterfall. Adopting it would mean either
(a) embedding the entire React + RxJS stack (the build step the
operator console *does not* have), or (b) bundling and re-exporting
their canvas implementation — non-trivial because the rendering is
tightly coupled to their Redux state shape.

Our waterfall covers: indent by depth, sort by start, bar in % of
trace span, ms label, error coloring. That's <80 LOC of imperative
DOM and is exactly what an operator needs to triage. The Grafana
Tempo waterfall (still available via ADR-0028 dashboards) is the
power-user escape hatch for trace_id-deep-dive features we don't
ship in v1 (span attributes side panel, span event timeline, etc.).

## Trade-offs

| ✓ | ✗ |
|---|---|
| Zero deps, zero build step — fits the operator-console ethos. | No span-attributes side panel (queued for E-4 if requested). |
| Reuses `/v1/query` — no new server surface for traces. | Browser-side rendering scales to a few thousand spans per trace before becoming sluggish; bigger traces still need Grafana Tempo. |
| Service map is computed from the same span result set — no second query. | Layout is circular; force-directed/dagre is deferred. |
| HiDPI-aware canvas — sharp on retina. | We own the renderer (≈140 LOC). |

## Service-map correctness notes

* Edges count *only* spans where parent.service ≠ child.service.
  Intra-service span hierarchy is internal detail.
* Nodes use the unique set of `service_name` values seen in the
  span set, so a trace with only one service draws one node and
  zero edges.
* Edge direction is always parent→child (downstream call), which
  matches the OTel semantic the user expects ("checkout-api calls
  payments-api"). Reverse-direction calls (B calls A while A is
  parent) are not modelled — they'd require linking via OTel
  `span.link` which we don't yet index.

## What ships

| File | Purpose |
|---|---|
| `services/ui-server/cmd/assets/waterfall.js` | `renderWaterfall` + `renderServiceMap`. ~140 LOC, zero deps. |
| `services/ui-server/cmd/assets/index.html` | `#panel-traces` with split list / detail layout. |
| `services/ui-server/cmd/assets/app.js` | Traces panel module — search via `/v1/query`, span detail per `trace_id`, click-to-load wiring. |
| `services/ui-server/cmd/assets/app.css` | `.traces-split` grid, `.span-row` styling, `.selected` row highlight. |

(All shipped in the E-1 commit because the SPA, CSS, and HTML for
all four E-1..E-4 tabs landed together so the operator could see
the surface evolving as features lit up.)

## Acceptance criteria

* Search returns up to 200 most recent traces for the given
  tenant/service/min-ms within 2 s.
* Clicking a trace renders the waterfall (<150 ms for a 100-span
  trace) and the service map.
* Status-non-OK spans render in `--err` colour.
* HiDPI displays render sharp service-map circles and arrowheads.
* No JS errors on traces with one span / one service / missing
  `parent_span_id` (root only).
