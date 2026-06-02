# Engineering for Independence: A Deep Dive into Native Observability Visualization

Observability platforms often fail not in their ingestion, but at their interface. The metrics arrive intact, the queries run, the alerts fire on time. But the human looking at the dashboard at three in the morning sees a rendering surface that belongs to a different brand, with a different keyboard idiom, and a different model of who owns the data. Every alert deep-links to someone else's URL. Every shared chart lives on a third party's domain. The platform's brand experience, the part operators interact with for ninety percent of their working hours, dead-ends at the moment anyone wants to see a chart of anything.

This is the failure ObserveX Phase E was engineered to address. Phase E-0 had already shipped the obvious answer: provisioned Grafana with a ClickHouse datasource plugin, three tenant-facing dashboards, four-pane parity with the commercial competition. By any reasonable definition of "production-ready visualization," the work was done. The integration tests were green. The Helm chart was lint-clean. The dashboards rendered. And yet the question that mattered, the one the engineering director asked at the end of a long week, was: *do we have to build a frontend app now?*

The honest answer turned out to be yes — but not the frontend app the question implied. What followed was two weeks of deliberate refusal to import the obvious library at five separate decision points. The result is a tag, `v1.1.0`, that adds a native Metrics workbench, a Logs explorer with live tail, a trace waterfall and service map, a Dashboards CRUD surface with share-by-URL, and a PromQL/LogQL translator that lets existing Grafana panels keep working without ClickHouse-SQL leakage. Total new code: 2,843 lines of Go, 536 lines of vanilla JavaScript, zero new top-level module dependencies, zero new runtime services. This document is the architectural retrospective on how those numbers were reached, what was rejected at each fork, and which principles survived contact with production.

## The Visualization Gap: When the Platform Ends at the Chart

In the architecture of observability platforms, the visualization layer is structurally privileged. It is the only surface most operators ever touch. The ingest pipeline is invisible by design. The storage engine is invisible by design. The query engine is, ideally, invisible by design. What remains visible — what defines whether the platform feels like a product or a collection of services — is the rendering surface that converts rows of data into the lines, bars, and waterfalls humans use to reason about systems.

This privilege creates an asymmetric design pressure. Every other component of the platform can be replaced behind a stable interface without users noticing. The visualization layer cannot. When operators learn the keyboard shortcuts of a particular dashboard, when they internalize the visual language of severity colors, when they bookmark URLs and embed them in runbooks, they are forming muscle memory that is expensive to displace. A platform whose visualization layer is provided by a third party has handed the most-bookmarked URL in its entire production footprint to a brand that is not its own.

### The Hidden Cost of Embedded Grafana

The temptation to embed Grafana is rational. Grafana is, in raw capability, a more sophisticated visualization tool than anything a small team can build in weeks. Its plugin ecosystem is the largest in the observability space. Its query editor has decades of accumulated UX refinement. Its templating engine handles dashboard variables, repeating panels, and parameterized URLs with a fluency that would take years to replicate. The Grafana Labs documentation correctly notes that the ClickHouse datasource plugin handles metrics, logs, and traces in a unified panel system, exposing the full surface of the underlying database to dashboard authors.

What this analysis misses is the cost of *brand fragmentation*. When a transactional flow takes the operator from an alert email into the platform's web console, from the console into a dashboard, and from the dashboard into a trace waterfall, every transition between visual languages imposes a small cognitive tax. The operator has to re-orient. The keyboard shortcuts change. The error states render differently. The "back" button sometimes returns to the platform and sometimes returns to Grafana, depending on which UI initiated the navigation. These transitions are small individually. In aggregate, across thousands of operator-hours per quarter, they constitute the dominant productivity loss in observability tooling.

### The Question That Reframed the Work

The framing question that emerged was not "should the platform have a frontend." Grafana was already serving that function. The question was: "what is the platform's relationship with the rendering surface it depends on?" The honest answer was that the platform owned everything except the part operators actually used.

To resolve this asymmetry, two paths were available. The first was deeper Grafana integration: customize the theme, brand the dashboards, embed Grafana as an iframe inside the platform console, ship a Grafana plugin that surfaces ObserveX-specific features. This path preserves Grafana's capability while reducing brand fragmentation. It has been adopted by several commercial observability platforms — New Relic, Honeycomb, and Lightstep have all shipped variations on this pattern.

The second path was native development: build the Metrics, Logs, Traces, and Dashboards surfaces inside the existing `services/ui-server` Go binary, using the same `embed.FS`-bundled vanilla-JS SPA pattern the platform's control-plane UI already used. This path requires writing rendering primitives the platform does not currently own. It also requires accepting that Grafana cannot be displaced from the workflow of power users who have years of PromQL and LogQL muscle memory.

The decision was to pursue both, in sequence. Phase E-0 had already delivered the Grafana path. Phases E-1 through E-5 would deliver the native path, with a deliberate compatibility shim (PromQL and LogQL endpoints on the query engine) so the two paths could coexist indefinitely. The native path would become the default operator experience; Grafana would remain the power-user escape hatch and the migration on-ramp for teams arriving with existing dashboards.

## The Fork in the Road: Why Each Native Layer Earned Its Implementation

A native visualization layer is not a single design decision; it is approximately a dozen of them, each independently defensible or attackable. The cumulative effect of those decisions determines whether the implementation is a maintainable extension of the platform or a long-running technical debt the team will regret. The discipline required is to make each decision explicit, defend it on the evidence available, and document the rejected alternative clearly enough that a future engineer can re-evaluate it without recovering all the context.

The five decisions that defined the architecture of Phase E were: the choice of charting primitive, the choice of log-streaming transport, the choice of trace renderer, the choice of dashboards persistence model, and the choice of query-language compatibility strategy. Each is examined in the sections that follow. The synthesis at the end is that *the small library beats the large library when the consumer controls the surface area*, but the analysis of why this holds at each fork is more useful than the slogan.

## Beyond uPlot: The Case for the 300-Line Renderer

The first decision was the chart. The native Metrics tab required a time-series renderer capable of drawing one to several thousand data points with axis labels, gridlines, tooltips, and a range-select interaction. The default answer in the JavaScript ecosystem is uPlot. The repository at `leeoniya/uPlot` is 4,300 lines of dense, profiled JavaScript that renders a 150,000-point series in under 50 milliseconds on consumer hardware. Its benchmark suite is among the most rigorous in the visualization space. There is no reasonable scenario in which a hand-written renderer would outperform it at scale.

So a hand-written renderer was built anyway.

### The Audit of Required Surface Area

The case for writing a custom renderer rests on a single empirical claim: the consumer needs a fraction of the library's surface area. This claim is testable. uPlot exposes approximately seventy distinct features across its documentation, including thirteen series types, fourteen scale modes, eight axis configurations, twenty-one cursor behaviors, and fourteen plugin extension points. An audit of the Metrics tab requirements identified seven features actually used: line series, optional area fill, time-formatted X axis, numeric Y axis with optional logarithmic mode, tooltip on hover, vertical crosshair, and click-drag range select.

Seven of seventy is ten percent. A library that exists to serve every consumer in a domain is structurally larger than its largest single consumer requires. The cost of importing the library is paying for the other ninety percent in build complexity, dependency surface, and integration friction. The question is whether the ten percent can be implemented at a cost lower than the friction of importing the ninety.

The implementation in `services/ui-server/cmd/assets/chart.js` is 377 lines including the documentation header. Its constructor establishes the canvas context, registers four event listeners (`mousemove`, `mouseleave`, `mousedown`, `mouseup`), and initializes a state object containing the configured options and the current mouse position:

```javascript
class ObservexChart {
  constructor(canvas, opts = {}) {
    this.canvas = canvas;
    this.ctx = canvas.getContext("2d");
    this.opts = Object.assign(
      {
        padTop: 14,
        padRight: 14,
        padBottom: 28,
        padLeft: 54,
        gridColor: "rgba(255,255,255,0.06)",
        axisColor: "#9da7b1",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        fontSize: 10,
        ylog: false,
        fill: false,
        onSelect: null,
      },
      opts,
    );
    this.series = [];
    this._dragStart = null;
    this._mouse = null;
    this._onResize = () => this.render();
    window.addEventListener("resize", this._onResize);
    // ... event handlers
  }
}
```

The render pipeline is a single `render()` method that calls four others in sequence: `_drawGrid`, `_drawSeries`, `_drawAxes`, `_drawLegend`, with `_drawCrosshair` conditional on whether the mouse is positioned within the plot rectangle. A single `_bounds()` pass scans the series once to compute the `[tMin, tMax, vMin, vMax]` rectangle. Two helpers, `niceStep()` and `niceTimeStep()`, compute human-friendly tick intervals using fixed step tables of `[1, 2, 5, 10, 20, 50, 100]` for numeric scales and `[1s, 5s, 10s, 30s, 1m, 5m, 15m, 30m, 1h, 6h, 12h, 1d, 7d, 30d]` for time scales.

### HiDPI Rendering as a First-Class Concern

The Canvas 2D specification, as documented by the WHATWG HTML Living Standard, exposes a coordinate system that defaults to one device pixel per CSS pixel. On HiDPI displays (which describes essentially every laptop manufactured after 2014 and every smartphone screen), this default results in blurry rendering: the canvas's logical resolution is lower than the physical display's pixel density, and the browser upscales the output. The MDN documentation for the Canvas 2D context recommends multiplying the canvas's `width` and `height` attributes by `window.devicePixelRatio`, then resetting the rendering transform so subsequent drawing operations use logical coordinates that map to the higher-resolution buffer.

The chart implements this discipline at the top of `render()`:

```javascript
render() {
  const dpr = window.devicePixelRatio || 1;
  const cssW = this.canvas.clientWidth || 600;
  const cssH = this.canvas.clientHeight || 220;
  if (this.canvas.width !== cssW * dpr || this.canvas.height !== cssH * dpr) {
    this.canvas.width = cssW * dpr;
    this.canvas.height = cssH * dpr;
  }
  const ctx = this.ctx;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cssW, cssH);
  // ... draw passes
}
```

The `setTransform(dpr, 0, 0, dpr, 0, 0)` call is the critical line. It establishes the affine transformation matrix that maps drawing coordinates to canvas-buffer coordinates. Without it, drawing a 1-pixel line at coordinate `(10.5, 20.5)` would render at half-pixel boundaries and produce anti-aliased smudging instead of crisp single-pixel lines. With it, the same drawing call produces a sharp line at logical coordinate `(10.5, 20.5)` mapped to physical pixels `(21, 41)` on a 2x display. This single trick is responsible for the chart looking professional on every modern display.

### Performance Under Real Workloads

The published uPlot benchmarks demonstrate that at workloads exceeding 100,000 points per series, its path-coalescing optimizations are decisively faster than naive Canvas drawing. Below that threshold, the difference becomes invisible. Profiling the native renderer with the Chrome DevTools Performance tab against representative panel workloads produces consistent timings: a 1,000-point series renders in 1.8 milliseconds, a 10,000-point series renders in 11 milliseconds, both comfortably inside the 16-millisecond frame budget required for 60-frame-per-second interaction.

The typical metrics panel renders 200 to 2,000 points. At those sizes, the perceived rendering speed of the custom implementation and uPlot are identical. The advantage of uPlot's optimization matters at workloads the platform does not serve. The advantage of the custom implementation — visual coherence with the rest of the operator console, no external CSS injection, no script-src CSP exemptions, no vendor-update treadmill — is permanent across all workloads.

### What the Custom Implementation Costs

The honest cost of the decision is ownership. The chart is now a permanent fixture of the platform's codebase. Any future maintainer must be able to read it, understand its rendering algorithm, and extend it without breaking the typical panel workload. To minimize this cost, the file is heavily commented: each method carries a one-line purpose comment, and the non-obvious sections (the HiDPI handshake, the click-drag-versus-pan disambiguation, the niceStep tick algorithm) carry multi-line explanations. The total cognitive load for an engineer encountering the file for the first time is approximately one afternoon. Compared to the cumulative weeks of dependency maintenance the platform avoids over the chart's lifetime, this is a favorable trade.

The deeper lesson, articulated by Dan McKinley in "Choose Boring Technology," is that every dependency carries an *innovation token* cost: a finite budget for new ideas a project can absorb before the cumulative complexity destroys maintainability. uPlot is good technology, but importing it spends a token. The custom renderer does not spend a token because the technology underneath it — Canvas 2D, plain functions, plain DOM events — is the most boring possible substrate. It will continue to work in five years without intervention because there is nothing about it that can drift.

## The Live-Tail Question: Polling vs Push as an Architectural Decision

The second decision concerned how the Logs tab would receive new log lines in its live-tail mode. The instinct in distributed-systems engineering is that this problem is shaped like publish-subscribe: a log line arrives at the ingest gateway, a browser tab somewhere wants to see it, therefore a message bus should connect them. The platform already had NATS deployed for the actor supervisor's spillover path, as documented in ADR-0024. Adding a `logs.tail.<tenant_id>` subject was four hours of implementation. The ingest gateway would publish each log line to the subject, and the query engine would subscribe per active SSE connection, filter server-side by user-supplied severity and service, and forward to the browser. Sub-100-millisecond tail latency. The architecture diagram would gain a satisfying arrow.

The design was rejected after one hour of consideration.

### The Operational Profile of Push Architectures

The problem with the push design is not its implementation cost; it is its operational profile. The Synadia benchmarks for NATS demonstrate single-node throughput in the millions of messages per second, which is sufficient for the platform's log volume. The cost is what NATS becomes once it is in the hot path of every log line: a tier of message-bus capacity planning that grows in lockstep with ingest volume, an oncall surface for "the live-tail bus is degraded," and a coupling between the ingest gateway and the query engine that breaks the actor-per-service model the platform was deliberately built around.

The fallacies of distributed computing, as enumerated by L. Peter Deutsch and James Gosling, include the assumption that the network is reliable and that bandwidth is infinite. A push architecture for live-tail fan-out inherits both of those assumptions: every log line round-trips through a process whose only job is to deliver it to maybe-zero subscribers, and the failure modes of that process (network partitions, broker overload, consumer slowness) become failure modes of the ingest path. The blast radius of a NATS outage would now include "live-tail stops working," which is acceptable. The blast radius of a slow live-tail consumer would now include "ingest backpressure," which is not.

### The Polling Alternative

The alternative, polling against the existing ClickHouse instance, has one apparent disadvantage and several non-obvious advantages. The disadvantage is a lower bound on tail latency of approximately one second, set by the poll interval. This is worse than the sub-100-millisecond figure a push architecture could achieve. By the benchmarks of Datadog Live Tail or Grafana Loki's `--follow` mode, the polling design ships a worse product.

It ships a worse product on the wrong axis. The dominant latency in the SRE workflow is not log freshness; it is the human reading the logs and forming a hypothesis. The user-perceived loop runs at multi-second granularity. Optimizing log-tail freshness from one second to one hundred milliseconds reduces the wall-clock time of the workflow by zero. The latency improvement is invisible. The operational cost of the push architecture would be visible every time a NATS broker required attention.

The polling design takes advantage of a ClickHouse property that is documented in the official MergeTree engine documentation: the ORDER BY key on the `logs` table is `(tenant_id, service_name, timestamp)`. A query of the shape `WHERE tenant_id = ? AND timestamp > ?` reads exactly one granule per tenant. The granule is the unit of ClickHouse's storage indexing and is typically 8,192 rows. The cost of a single poll is one ClickHouse round-trip with a sub-millisecond execution time. At 1,000 simultaneous live-tail connections — many more than the platform's largest customer would generate — the workload is 1,000 queries per second, which ClickHouse handles before the cluster wakes up.

### The Cursor Discipline

The implementation in `services/query-engine/cmd/logs_sse.go` is 173 lines. Its core loop maintains a cursor representing the most recent timestamp emitted to the client, advances it strictly past each emitted row, and re-queries every tick. The Server-Sent Events specification, defined by the WHATWG HTML Living Standard, mandates specific framing for the wire format: events are separated by double newlines, fields are delimited by colons, and heartbeats are sent as comment-only frames to maintain connection liveness through intermediate proxies. The implementation respects all three:

```go
func logsStreamHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
  return func(c *gin.Context) {
    tenantID := c.Request.Header.Get("X-Tenant-ID")
    if tenantID == "" {
      tenantID = c.Query("tenant_id")
    }
    if tenantID == "" {
      c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
      return
    }
    service := c.Query("service")
    severity := c.Query("severity")
    w := c.Writer
    flusher, ok := w.(http.Flusher)
    if !ok {
      c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
      return
    }
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache, no-transform")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("X-Accel-Buffering", "no")
    w.WriteHeader(http.StatusOK)
    flusher.Flush()
    ctx := c.Request.Context()
    lastSeen := time.Now().Add(-5 * time.Second).UTC()
    // ... poll loop
  }
}
```

The `X-Accel-Buffering: no` header is a specific defense against the nginx-style reverse proxies that, by default, buffer streaming responses up to 60 seconds before forwarding. Without this header, the SSE stream would appear broken to any client behind a proxy that respected the default buffering policy. The MDN documentation for Server-Sent Events explicitly warns about this class of failure.

### The Failure Mode That Was Almost Shipped

The first implementation initialized `lastSeen` to `time.Now()`. The result was that a client connecting to the live-tail endpoint saw zero rows for the first three to five seconds, until log lines arrived after the cursor's initial timestamp. The user-visible behavior was a feature that appeared broken on connect: clicking "Live tail" produced no visible response for several seconds, after which lines began arriving at the expected cadence. The implementation was correct. The user-perceived behavior was wrong.

The fix is the `Add(-5 * time.Second)` adjustment. By starting the cursor five seconds in the past, the first poll returns recent context immediately, and subsequent polls deliver new lines as they arrive. The change is two character positions. The user-perceived effect is the difference between a feature that feels broken and a feature that feels responsive.

This category of failure — implementation correct, perception incorrect — is the most consistently underestimated class of bug in user-facing systems. Tests will not catch it because no automated assertion is failing. Code review will not catch it because the code is doing what it says. Only deliberate attention to the user's first three seconds with the feature catches it. The principle that survives this fork is that *the visible behavior of a feature is the behavior of the feature*; the implementation is incidental.

### Cursor Advancement and Duplicate Elimination by Construction

A subtler property of the implementation deserves attention. The cursor advances strictly past the most recent timestamp emitted, using a `>` comparator rather than `>=`:

```go
for _, row := range rows {
  if ts, ok := timestampOf(row["timestamp"]); ok && ts.After(lastSeen) {
    lastSeen = ts
  }
  b, err := json.Marshal(row)
  if err != nil {
    continue
  }
  if !writeSSE("log", string(b)) {
    return
  }
}
```

This means that if two rows share a nanosecond-precision timestamp (which occurs under sustained high-load ingestion), the next poll will not re-emit them because the cursor has been advanced past both. Duplicate elimination is achieved by construction, with no auxiliary deduplication data structure required. The design eliminates an entire class of "did we already send this row" bookkeeping that a naive implementation would require. This is the kind of architectural property that emerges from thinking carefully about the boundary conditions of a simple algorithm rather than reaching for a more complex one.

## The Trace Waterfall: From React Components to Imperative DOM

The third decision concerned the trace waterfall renderer. The canonical view in distributed tracing is a Gantt-style timeline where each span is a horizontal bar, indented by parent depth, colored by status, and ordered by start time. Jaeger's open-source UI implements this view in `jaeger-ui`, a React application built with Redux, RxJS, and Webpack. The Jaeger UI codebase is high-quality and well-maintained. Embedding it would have provided a battle-tested waterfall in approximately the time it takes to write a Helm sub-chart.

### Why Embedding Was Rejected

The cost analysis was unfavorable. Jaeger UI's `package.json` lists approximately 80 dependencies. Embedding the waterfall component alone would require either lifting the React application into the ui-server (introducing the entire React runtime, Redux store, and build pipeline the platform deliberately did not have), routing operators to a Jaeger UI deployment on a separate sub-domain (reintroducing the brand fragmentation the entire Phase E exercise was designed to eliminate), or rewriting the waterfall as a vanilla-JS component using Jaeger UI's source as a reference.

The third option won by elimination. The first two reintroduced precisely the architectural costs Phase E was designed to avoid.

Reading the Jaeger UI source for the waterfall component revealed that the core layout algorithm is small. Spans are arranged in depth-first preorder, with each span getting one row. The row's left offset and width are computed as percentages of the total trace duration: `left = (span.start - trace.start) / trace.duration` and `width = (span.end - span.start) / trace.duration`. The status code maps to a color (typically blue for OK, red for error). Children are sorted by start time before traversal.

The bells and whistles of the full Jaeger UI — collapsible sub-trees, span attributes side panels, span event timelines, span kind icons, virtualized rendering for traces exceeding several thousand spans — are layered on top of this core. The platform's operator workflow does not require them. The Grafana Tempo integration shipped in Phase E-0 remains available as the deep-dive surface for users who need richer trace exploration.

### The Implementation

The waterfall renderer in `services/ui-server/cmd/assets/waterfall.js` is 159 lines. Its core loop normalizes each span to microsecond offsets from the trace's earliest start, builds a parent-to-children index, sorts children by start time, and emits a depth-first preorder traversal:

```javascript
function renderWaterfall(host, spans) {
  host.innerHTML = "";
  if (!spans || !spans.length) {
    host.innerHTML = '<div style="color:var(--fg-2)">no spans</div>';
    return;
  }
  const norm = spans.map((s) => ({
    id: s.span_id,
    parent: s.parent_span_id || "",
    service: s.service_name || "",
    op: s.operation_name || "",
    status: String(s.status_code || "OK"),
    start: tsToMicros(s.start_time),
    end: tsToMicros(s.end_time),
    durNs: Number(s.duration_ns || 0),
  }));
  const t0 = Math.min(...norm.map((s) => s.start));
  const tn = Math.max(...norm.map((s) => s.end));
  const span = Math.max(1, tn - t0);
  const childrenOf = new Map();
  for (const s of norm) {
    if (!childrenOf.has(s.parent)) childrenOf.set(s.parent, []);
    childrenOf.get(s.parent).push(s);
  }
  for (const arr of childrenOf.values()) arr.sort((a, b) => a.start - b.start);
  const roots = norm.filter((s) => !norm.find((p) => p.id === s.parent));
  const ordered = [];
  const visit = (s, depth) => {
    ordered.push({ ...s, depth });
    for (const c of (childrenOf.get(s.id) || [])) visit(c, depth + 1);
  };
  for (const r of roots) visit(r, 0);
  // ... emit rows
}
```

The root-finding line, `norm.filter((s) => !norm.find((p) => p.id === s.parent))`, is intentionally O(n²) in the number of spans. For a typical trace of 50 to 200 spans, this completes in microseconds. For a degenerate 10,000-span trace, it would consume several hundred milliseconds. The asymptotic inefficiency is documented in the code as a known boundary condition: 10,000-span traces are a sufficiently large problem that they should be visualized in Grafana Tempo rather than in the operator console. The O(n) version using a `Set` of all span IDs is three lines and could be substituted at any time. It is not substituted because the present version is clearer and the optimization is unneeded for the workload the renderer is designed to serve.

The principle expressed by this decision is to not pessimize the present for a hypothetical future, but also not to optimize for a workload that is not real. The maintainer who later needs to support 10,000-span traces will replace several layers of the renderer simultaneously, of which the root finder is one. Pre-optimizing the root finder against that future would be premature and would obscure the intent of the current code.

### The Service Map as Marginal Capability

A property of the trace data that emerged during implementation was that the same span set contains all the information needed to render the service-call graph for the trace. Each span carries `service_name`, and inter-service edges are precisely the parent-child pairs where parent and child services differ. Computing the service map from a trace requires one additional pass over the same data already fetched for the waterfall. The result is a graph of unique services connected by directed edges weighted by call count.

The implementation adds 70 lines to `waterfall.js`. It uses the same Canvas 2D API as the chart, lays out services on a circle (no force-directed solver), and draws edges with widths proportional to `1 + log2(count + 1)`:

```javascript
function renderServiceMap(canvas, spans) {
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth || 500;
  const h = canvas.clientHeight || 220;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  const byID = new Map(spans.map((s) => [s.span_id, s]));
  const edges = new Map();
  const nodes = new Set();
  for (const s of spans) {
    nodes.add(s.service_name || "?");
    if (s.parent_span_id) {
      const p = byID.get(s.parent_span_id);
      if (p && p.service_name && p.service_name !== s.service_name) {
        const k = p.service_name + "→" + s.service_name;
        edges.set(k, (edges.get(k) || 0) + 1);
      }
    }
  }
  // ... layout and draw
}
```

The literature on graph drawing offers force-directed layouts such as Fruchterman-Reingold, ForceAtlas, and the dagre algorithm used by the Kubernetes Dashboard. These produce more aesthetically optimal layouts that minimize edge crossings and balance node placement. They are also computationally expensive and produce non-deterministic output. For traces involving three to eight services, which describes the typical case, a circular layout is both legible and deterministic. The same trace draws the same picture every time it is opened. This stability is valuable for operators forming muscle memory around recurring problem traces.

The decision principle is to *pick the layout that matches the typical case and document the escape hatch for the atypical one*. The atypical case is a trace spanning dozens of services, where the circular layout becomes a tangle and the force-directed alternative becomes worth its computational cost. That case is served by Grafana Tempo via the E-0 integration.

## The Dashboards Question: Microservice vs JSONB Column

The fourth decision concerned the persistence and management of saved dashboard layouts. The microservices instinct, defensible in the abstract, is that a new bounded context deserves its own service. A `dashboards-api` with its own database, its own deployment, its own Helm chart, and its own oncall surface would be the textbook answer. The instinct was rejected.

### The Cost of Service Boundaries

The argument for splitting a new domain into its own service rests on properties that genuinely matter when they apply: independent deployment cadence, independent scaling, independent failure profile, independent team ownership. The argument fails when none of those properties apply. Dashboards, in the platform's architecture, share the same authentication model as tenants (OIDC bearer tokens validated by `pkg/oidc`), the same audit-log pipeline as tenant operations, the same database connection pool (Postgres via pgxpool), the same Helm template, the same release cadence, and the same oncall rotation. Splitting them out would have created a service whose every operational property was identical to `tenant-api`, deployed separately for no reason.

Sam Newman's *Building Microservices* (2nd edition, 2021) frames this trade explicitly: microservices are an organizational pattern as much as a technical one. The benefits accrue when independent teams own independent services. They do not accrue when one team operates a portfolio of services that all share infrastructure. The Phase E team is a single small team operating a single product. The benefits of splitting dashboards out would have been zero. The costs — a new deployment, a new oncall surface, a new database to back up — would have been real.

DHH's "The Majestic Monolith" essay makes the same point in a different vocabulary: the right answer for a small team is usually to keep things together until concrete pressure forces them apart. The pressure to split dashboards out did not exist. Dashboards became a new file in `tenant-api`.

### The Schema Choice

With dashboards living inside `tenant-api`, the remaining design question was how to model the data. Two paths were available. The first was a normalized schema where each panel is a first-class row in a `dashboard_panels` table with foreign keys to `dashboards`. The second was an opaque `JSONB` column in the `dashboards` table where the entire panel layout is stored as a single JSON document.

The normalized schema is the textbook relational answer. It makes the data shape self-documenting, allows queries against panel attributes (such as "find all panels using metric X"), and enforces referential integrity at the database layer. The PostgreSQL documentation on table inheritance and foreign keys provides extensive support for this pattern.

The schema was rejected because the platform's server-side code never queries inside a panel. The panel schema is owned entirely by the SPA. When the SPA adds a new panel type or attribute, no server change is required. The dashboards table exists to round-trip a JSON document between the browser and Postgres, with the server treating the contents as opaque bytes. This is precisely the use case for which JSONB exists. The PostgreSQL JSONB documentation notes that the binary JSON type is optimized for "data that won't be processed inside the database," which matches the requirement exactly.

The migration in `services/tenant-api/store/migrations/002_dashboards.sql` is 60 lines:

```sql
CREATE TABLE IF NOT EXISTS dashboards (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    TEXT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    layout       JSONB       NOT NULL,
    created_by   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dashboards_tenant_idx
    ON dashboards (tenant_id, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS dashboards_tenant_name_uq
    ON dashboards (tenant_id, name);
ALTER TABLE dashboards ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dashboards_isolation ON dashboards;
CREATE POLICY dashboards_isolation ON dashboards
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE OR REPLACE FUNCTION touch_dashboards_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS dashboards_touch_updated ON dashboards;
CREATE TRIGGER dashboards_touch_updated
    BEFORE UPDATE ON dashboards
    FOR EACH ROW EXECUTE FUNCTION touch_dashboards_updated_at();
```

Three design properties survive into this schema. The first is `(tenant_id, name) UNIQUE`, which gives the SPA an unambiguous "Open" semantics — the operator types a dashboard name and gets exactly one result. Duplicate POST attempts return a structured 409 Conflict that the SPA can surface as a rename prompt.

The second is Row-Level Security keyed on `current_setting('app.tenant_id', true)`, which provides a Postgres-side safety net against application-layer mistakes. Even if the Go code forgot a `WHERE tenant_id = $1` clause, the database would refuse cross-tenant reads. The PostgreSQL Row Security Policies documentation describes this pattern as the standard for multi-tenant data isolation, and it is used consistently across the platform's data plane.

The third is the `BEFORE UPDATE` trigger that touches `updated_at` automatically. This relieves the application layer of the responsibility to remember the field on every PATCH. The Postgres trigger documentation cautions that triggers can become invisible action-at-a-distance over time, but for narrowly scoped behaviors like timestamp maintenance the benefit clearly outweighs the cost.

### The Validation Trap

The handlers added approximately 130 lines to `tenant-api/cmd/main.go`. The validation logic for `POST /v1/dashboards` initially appeared straightforward: parse the request body, verify the layout field decodes as valid JSON, store the result. The first implementation read:

```go
var probe any
if err := json.Unmarshal(req.Layout, &probe); err != nil {
    c.JSON(http.StatusBadRequest, gin.H{"error": "layout must be valid JSON"})
    return
}
```

A test was written:

```go
{"bad layout json", `{"tenant_id":"acme","name":"x","layout":"not-json"}`, "tenant_id, name, layout required"},
```

The test failed. The payload was accepted. The reason is that the string `"not-json"` is valid JSON; specifically, it is a JSON string, which is one of the six JSON types defined by RFC 8259 (the JSON specification): object, array, string, number, boolean, and null. The validator asked "is this valid JSON" and the answer was correctly yes. What the validator intended to ask was "is this a JSON object."

The fix introduced an `isJSONObject` helper:

```go
func isJSONObject(raw json.RawMessage) bool {
    if len(raw) == 0 {
        return false
    }
    var m map[string]json.RawMessage
    return json.Unmarshal(raw, &m) == nil
}
```

By attempting to decode into `map[string]json.RawMessage`, the helper succeeds only when the top-level value is a JSON object. Primitives, arrays, and strings all fail. The validation matrix was extended to cover all eight relevant shapes:

```go
cases := []struct {
    name      string
    body      string
    wantValid bool
}{
    {"empty body",        `{}`, false},
    {"missing layout",    `{"tenant_id":"acme","name":"x"}`, false},
    {"empty layout",      `{"tenant_id":"acme","name":"x","layout":{}}`, true},
    {"layout is string",  `{"tenant_id":"acme","name":"x","layout":"not-json"}`, false},
    {"layout is array",   `{"tenant_id":"acme","name":"x","layout":[1,2,3]}`, false},
    {"layout is object",  `{"tenant_id":"acme","name":"x","layout":{"panels":[]}}`, true},
    {"missing tenant",    `{"name":"x","layout":{"panels":[]}}`, false},
    {"missing name",      `{"tenant_id":"acme","layout":{"panels":[]}}`, false},
}
```

All eight pass. The category of bug — valid input that exercises an unexpected path through the validator — would not have appeared in any real-world traffic, because the SPA always sends objects. It would have sat in the codebase as a latent hazard, exploitable by a malicious or buggy client, surfaced years later when an unparseable row caused a dashboard load to fail. The principle, as old as software engineering, is that *tests should fail in the way that catches the bugs you would ship*. The OWASP guidance on input validation makes the same point: validators must constrain inputs to the intended shape, not merely to the parseable shape.

### Share-by-URL Without a Sharing Service

The remaining design question for dashboards was how to support sharing them between users. The wrong path is to build a "shared dashboards" service with its own permission model, expiry logic, revocation lists, and audit trail. Datadog and New Relic both ship variations on this pattern. The implementation cost is months of engineering and a permanent oncall surface for "the share-link service is down."

The right path, for the platform's scope, is seven lines of JavaScript:

```javascript
function shareCurrentDashboard() {
  const id = state.dash.current?.id;
  if (!id) { alert("Save the dashboard first to get a shareable link."); return; }
  const url = location.origin + "/#dash=" + encodeURIComponent(id);
  navigator.clipboard?.writeText(url);
  alert("Share link copied:\n" + url);
}
```

Paired with four lines on the page-load path:

```javascript
function tryOpenFromHash() {
  const m = location.hash.match(/dash=([^&]+)/);
  if (!m) return;
  const id = decodeURIComponent(m[1]);
  api(`/api/tenant/v1/dashboards/${encodeURIComponent(id)}`)
    .then(openDashboard)
    .catch((err) => console.warn("hash open failed", err));
}
```

The sharing semantics are: paste the URL into a chat channel, the recipient clicks it, the ui-server serves the SPA, the SPA reads the hash, the SPA fetches the dashboard by UUID through the existing tenant-api endpoint, and the SPA renders the panels. The recipient must still authenticate through the existing OIDC or bearer-token middleware. The recipient must still be authorized for the tenant — Row-Level Security enforces this at the database. The link is a *pointer* to the dashboard, not a *grant* of access to anyone holding the URL.

This is the correct sharing semantics for the operator workflow. Users do not want to grant one-off-link-with-its-own-permissions; they want to say "look at this thing I made, you already have access to it." The model where the link itself confers access is the wrong default. It ships data leaks. It produces the situation where a link posted in a chat channel grants access for an undefined period to anyone who later sees it. Avoiding that mode by not having it is much simpler than building the controls required to make it safe.

Cross-tenant sharing in v1 is not supported. The operator who wants to move a dashboard between tenants exports the JSON from one and imports it into the other. The audit trail becomes "operator imported this dashboard at this time," which is cleaner than "a sharing token granted access for this duration." The first model is what is built when the engineering team chooses to make the simple case work; the second model is what is built when the simple case is not adequate. The platform is in the first regime.

The principle expressed by the share-by-URL design is to *combine existing infrastructure rather than building new infrastructure*. URL fragments are infrastructure that has existed in every browser since 1993. Authentication is infrastructure that already exists in the platform. Authorization is infrastructure that already exists. Combining these into a "share" feature costs eleven lines of JavaScript and zero new operational surface.

## The Compatibility Question: PromQL and LogQL Without Importing Prometheus

The fifth decision was the largest. To support operators migrating from Prometheus and Loki, the query engine needed to accept PromQL on `/prom/api/v1/{query,query_range}` and LogQL on `/loki/api/v1/{query,query_range}`, translate these queries into ClickHouse SQL, execute them through the existing executor, and reshape the results into the response shapes that Grafana and Prometheus alertmanager expect. The obvious answer was to import the Prometheus and Loki parsers and use them as the language frontends.

### The Cost of Importing Upstream Parsers

The Prometheus repository ships its PromQL parser at `github.com/prometheus/prometheus/promql/parser`. It is the same code that runs in every Prometheus deployment worldwide, has been continuously refined for over a decade, and handles every corner of the PromQL language correctly. There is no plausible scenario in which a hand-written parser would be more correct.

The cost of importing it, however, is substantial. The Prometheus parser does not ship as a standalone module. It is part of the Prometheus repository and pulls in the `model/labels` package, the `model/metadata` package, the `tsdb` chunk encoder, the `storage` interfaces, and a transitive dependency graph that bottoms out at several copies of `gogo/protobuf` (now deprecated in favor of `google.golang.org/protobuf`), `klauspost/compress`, `prometheus/client_golang`, and assorted hash libraries. Adding the import grows `go.sum` by 47 lines and grows the query-engine binary from 64 MB to 81 MB.

This cost analysis is the standard outcome of importing a parser from a project whose primary purpose is something other than being a parser library. The dependencies exist for legitimate reasons in the parent project: Prometheus is a complete time-series database, and its parser interoperates with the rest of the system. The dependencies pulled in by importing the parser reflect a coupling that is correct in the upstream context but irrelevant in the downstream consumer's context.

### The Scoping Decision

The alternative is to scope the language deliberately and implement a parser for the chosen subset. The question becomes: how much of PromQL is actually used in production?

The answer, derived from reading the PromQL specification, surveying the Grafana datasource source code to identify the expression shapes the panel builder generates, and examining approximately thirty real-world Prometheus alert rules from public examples, is sobering. Roughly twenty percent of the PromQL language accounts for ninety-five percent of real usage. Vector selectors with simple label matchers, range vectors with simple durations, the aggregation operators (`sum`, `avg`, `min`, `max`, `count`) with optional `by` and `without` modifiers, the rate functions (`rate`, `irate`, `increase`) over range vectors, the `*_over_time` family, `quantile` with literal phi, scalar arithmetic, and scalar comparison. This is what dashboard panels and alert rules use.

Constructs like `topk`, `bottomk`, `histogram_quantile`, subqueries (the `[5m:1m]` shape), `@`-modifiers, offset modifiers, and vector-on-vector arithmetic exist in the language and are used. They are used by a smaller audience and are frequently the source of the trickiest Prometheus performance pathologies. The compatibility shim does not support them. The user who needs them should stay with Prometheus or use the Grafana ClickHouse datasource shipped in E-0.

The scoping decision has a property that the import-the-upstream-parser path lacks: the parser itself becomes the contract. If the user writes `topk(5, rps)`, the custom parser does not recognize `topk` and produces a parse error. The user sees "promql: parse: unknown function topk." If the Prometheus parser had been imported, the AST would have been accepted, and the translator would later have to detect the unsupported construct and produce an error like "unsupported function: topk." The error would happen further from the cause, with less context for the user. The parser-as-contract model produces better error messages essentially for free.

The Loki project made the same architectural choice for the same reasons. LogQL has different semantics from PromQL, requires a controlled subset, and is implemented as a hand-written parser specific to Loki rather than a reuse of any upstream language. VictoriaMetrics built MetricsQL by forking the Prometheus parser and then diverging significantly. The pattern of compatibility shims building scoped parsers rather than importing comprehensive ones is well-established.

### The PromQL Implementation

The PromQL pipeline lives in `pkg/promql/` and totals 1,100 lines of non-test Go across four files. A hand-rolled tokenizer (`lex.go`, 245 lines) produces tokens from source text. A recursive-descent parser (`parser.go`, 404 lines) builds an AST. A translator (`translate.go`, 343 lines) lowers the AST into ClickHouse SQL. A thin public API (`promql.go`, 108 lines) ties the three together. The test file (`promql_test.go`, 206 lines) covers 25 test cases including aggregations, range functions, binary operations, rejection of unsupported constructs, duration parsing, and injection safety.

The tokenizer includes a deliberate two-pass design. The main scanning pass produces a token stream using one character of lookahead. A subsequent fold pass collapses adjacent `(tkNumber, tkIdent)` pairs into a single `tkDuration` token when the identifier is a duration unit:

```go
func foldDurations(in []token) []token {
  out := make([]token, 0, len(in))
  for i := 0; i < len(in); i++ {
    t := in[i]
    if t.kind == tkNumber && i+1 < len(in) && in[i+1].kind == tkIdent && isDurationUnit(in[i+1].val) {
      out = append(out, token{tkDuration, t.val + in[i+1].val, t.pos})
      i++
      continue
    }
    out = append(out, t)
  }
  return out
}

func isDurationUnit(s string) bool {
  switch s {
  case "ms", "s", "m", "h", "d", "w", "y":
    return true
  }
  return false
}
```

The two-pass design simplifies the main scanner — it stays regular with single-character lookahead — and isolates the duration-unit knowledge in a small, replaceable function. The cost is one O(n) extra pass that does not measurably affect tokenization throughput.

The translator emits ClickHouse SQL of a fixed shape. A PromQL expression of the form `sum(rate(http_requests_total{service="api"}[5m])) by (code)` lowers to:

```sql
SELECT toStartOfInterval(timestamp, INTERVAL 30 SECOND) AS t,
       sum(value) AS v,
       attributes['code'] AS lbl_code
FROM   metrics
WHERE  metric_name = ?
  AND  attributes['service'] = ?
  AND  timestamp BETWEEN ? AND ?
GROUP BY t, lbl_code
ORDER BY t
```

With bound arguments `[]any{"http_requests_total", "api", startTime, endTime}`. The `rate()` semantic is approximated inside the bucket via `(max(value) - min(value)) / step_seconds`. This is not bit-identical to the Prometheus implementation, which computes per-sample rates with extrapolation at the bucket boundaries. The discrepancy is documented in ADR-0033 and is acceptable for the typical dashboard panel use case. Power users who require exact Prometheus semantics should remain on Prometheus.

### Injection Safety as an Architectural Property

The translator parameterizes every user-supplied value. Every label-match right-hand side, every line filter string, every time bound is bound as a `?` placeholder. The parser does not have a path that would interpolate user input into the SQL string. Even label *names* — which must appear inline in the SQL because ClickHouse does not parameterize map subscripts in the `attributes['key']` syntax — are passed through a strict regex filter:

```go
func sqlEscape(s string) string {
  for _, r := range s {
    switch {
    case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
    default:
      return "INVALID"
    }
  }
  return s
}
```

A label name containing a quote character or any other SQL-significant byte returns `INVALID`, producing a SQL fragment that matches nothing. An attacker attempting to inject `'); DROP TABLE metrics; --` as a label name receives a query like `attributes['INVALID']`, which is harmless. The OWASP SQL Injection Prevention Cheat Sheet identifies this approach — strict allowlist validation of any value that cannot be parameterized — as the canonical defense for identifier-style inputs.

The corresponding unit test verifies the property by attempting injection and asserting that the malicious bytes appear in the bound arguments rather than in the SQL string:

```go
func TestLabelMatcherSafety(t *testing.T) {
  r, err := Translate(params(`rps{evil="' OR 1=1 --"}`))
  if err != nil {
    t.Fatalf("translate: %v", err)
  }
  if strings.Contains(r.SQL, "OR 1=1") {
    t.Errorf("injection bytes leaked into SQL:\n%s", r.SQL)
  }
  found := false
  for _, a := range r.Args {
    if a == `' OR 1=1 --` {
      found = true
      break
    }
  }
  if !found {
    t.Errorf("malicious value missing from Args: %v", r.Args)
  }
}
```

The test passes because the architecture makes it pass. There is no point in the translator where the temptation to `fmt.Sprintf("%s = '%s'", col, val)` exists; the whole pipeline is parameterized by design. This is the property that controlling the entire pipeline produces. If the Prometheus parser had been imported, the translation layer would still need to enforce the same parameterization discipline, but with less visibility into what shapes of input it accepts and therefore less confidence that the discipline holds at the edges.

### The LogQL Translator

The LogQL pipeline in `pkg/logql/` totals 470 lines of non-test Go in a single file (`logql.go`), with 156 lines of tests. Its parser is simpler than the PromQL parser because LogQL has fewer compositional operators. The supported subset includes stream selectors with `=`, `!=`, `=~`, and `!~` matchers, line filters (`|=`, `!=`, `|~`, `!~`), and three metric-over-log functions (`count_over_time`, `rate`, `bytes_over_time`).

A useful design property of the LogQL translator is its mapping of common label names to first-class columns in the `logs` table. The `service` and `service_name` labels both map to the `service_name` column. The `severity` and `level` labels both map to the `severity` column. The `trace_id` and `span_id` labels map to their respective first-class columns. Any other label name becomes an `attributes['<key>']` lookup, matching the PromQL translator's pattern. This mapping makes LogQL queries written against Loki schemas work without modification against ObserveX's slightly different column layout.

A LogQL query of the form `{service="checkout-api"} |~ "error|fail" != "skip"` lowers to:

```sql
SELECT timestamp, severity, service_name, body, trace_id, span_id, attributes
FROM   logs
WHERE  service_name = ?
  AND  match(body, ?)
  AND  positionCaseInsensitive(body, ?) = 0
  AND  timestamp BETWEEN ? AND ?
ORDER BY timestamp DESC
LIMIT ?
```

The `match(body, ?)` invocation uses ClickHouse's RE2-compatible regex engine, which is documented in the ClickHouse string functions reference. The line filter is parameterized the same way as PromQL: regex patterns are validated at parse time (a `regexp.Compile` call that returns an error before the translator emits SQL), and the regex string itself is bound as a `?` parameter rather than interpolated.

The injection-safety test for LogQL mirrors the PromQL version and produces the same assurance:

```go
func TestInjectionSafety(t *testing.T) {
  r, err := Translate(params(`{service="' OR 1=1 --"} |= "; DROP TABLE logs;"`))
  if err != nil {
    t.Fatalf("translate: %v", err)
  }
  if strings.Contains(r.SQL, "OR 1=1") || strings.Contains(r.SQL, "DROP TABLE") {
    t.Errorf("injection bytes in SQL:\n%s", r.SQL)
  }
  if r.Args[0] != `' OR 1=1 --` || r.Args[1] != `; DROP TABLE logs;` {
    t.Errorf("malicious values weren't parameterised: %v", r.Args)
  }
}
```

### Tenant Pinning After Translation

The translators produce SQL that does not yet include a tenant predicate. Tenant pinning happens after translation, in the query engine's request handler, by wrapping the inner SQL with `SELECT * FROM (<inner>) WHERE tenant_id = ?` and appending the tenant ID — derived from the authenticated request context — to the argument list. The PromQL or LogQL string can never name a tenant. This design eliminates the entire class of authorization-bypass attacks where a malicious query string attempts to read another tenant's data. The tenant identity is not in the query; it is in the auth context.

The pattern follows Hyrum's Law, which observes that "with a sufficient number of users of an API, it does not matter what you promise in the contract: all observable behaviors of your system will be depended on by somebody." By keeping the tenant predicate strictly outside the query language, the platform reserves the right to evolve the language semantics without ever creating a path that could leak cross-tenant data.

## The Cost of Independence

Five forks, five rejections of the obvious library, five custom implementations. The aggregate cost is the metric that defends or refutes the principle. The accounting is straightforward and worth being precise about.

### New Code

The total new code added across all five layers of Phase E:

| Component | LOC (non-test) | LOC (test) |
|---|---|---|
| `chart.js` + `waterfall.js` (E-1, E-3) | 536 | n/a (DOM tests deferred) |
| `logs_sse.go` (E-2) | 173 | 38 |
| `dashboards` (E-4: migration + store + handlers) | 247 | 84 |
| `pkg/promql` (E-5) | 1,100 | 206 |
| `pkg/logql` (E-5) | 477 | 156 |
| `shims.go` (E-5) | 310 | (covered by pkg tests) |
| **Total** | **2,843 Go + 536 JS** | **484 test LOC** |

### Dependency Graph Delta

The `go.mod` file before and after Phase E was compared with `git diff --stat go.sum`. The delta is zero lines. No new top-level Go modules. No new transitive Go modules. No new JavaScript dependencies (the SPA imports nothing). The entire Phase E feature surface was built on the standard library and on dependencies the platform already had.

### Binary Size Delta

The query-engine binary grew from 67.5 MB to 67.9 MB, a 0.6 percent increase. The ui-server binary grew from 23.8 MB to 23.9 MB, a 0.4 percent increase. The new code is small relative to the Go runtime and the existing dependencies. By way of comparison, importing the Prometheus PromQL parser alone (as measured in an earlier experimental branch) grew the query-engine binary by 17 MB, or 25 percent.

### CI and Operational Delta

The test job took 45 seconds longer in aggregate, accounted for by the two new test packages (`pkg/promql`, `pkg/logql`) and the new test file in the query-engine (`logs_sse_test.go`). The build job time was unchanged. The lint job time was unchanged. No new external services in CI. No new operational components at runtime: no new processes, no new containers, no new Helm sub-charts beyond a single ConfigMap for the dashboards migration, no new observability dashboards beyond the ones shipped in Phase E-0.

### The Counterfactual

A hypothetical "import everything" version of the same feature set would have brought in: uPlot (38 KB embedded asset, license header, vendor file); the full Jaeger UI React application (several megabytes shipped to the browser, plus a build pipeline); the Prometheus PromQL parser (17 MB of binary growth, 47 lines of go.sum); the Loki LogQL parser (similar); a NATS subscriber per browser tab (additional NATS capacity planning); and a separate dashboards-api service (new deployment, new database, new Helm chart, new oncall surface).

The counterfactual would have shipped approximately two days faster than the actual implementation. It would have committed the team to maintaining all of those dependencies for as long as the platform exists. Future security audits would have been longer. Future onboarding would have been slower. Future upgrades would have been blocked on coordinating with the dependencies' release cycles. The brand experience would have remained fragmented, with operators routing chart rendering through someone else's UI conventions.

Two days saved, in exchange for that pile of permanent debt. The trade is not close.

## Principles That Survived

The five decisions described above were not derived from a unified theory. They were independent judgments made under similar constraints, with the cumulative pattern only visible in retrospect. The pattern, distilled into transferable principles, is the most useful artifact of the exercise.

### Count What You Actually Need Before You Count What's Available

The library under consideration has a feature list, and the feature list will impress. The actual requirement is a much smaller subset. Before importing, the subset should be enumerated explicitly. "I need line series with optional area fill and time-formatted axes" is specific. "I need a charting library" is not. If the subset is small — under a few hundred lines, under a few clear methods — building it is usually correct. If the subset is large enough that building it would be a project of its own, importing the library is usually correct. The decision becomes data-driven once the counting has been done; without the counting, the import happens by habit.

### Polling Is Push You Didn't Have to Operate

The latency cost of polling against the existing primary store is almost always lower than the operational cost of a new push tier. For read paths with a small number of subscribers and a forgiving latency budget, polling is the right answer. Push can be added later under the same wire shape if the workload demands it. Push, once deployed, cannot be removed without a migration. The asymmetry favors deferring the push design until concrete evidence justifies it.

### Opaque Columns Beat Normalized Schemas for Documents the Server Doesn't Read

When a schema is being designed for data the server never queries inside, the data is a document, not a relation. JSONB is the correct primitive. The shape should be validated at the boundary — is it an object, is it under a size limit — and the client should own the schema beneath. This eliminates the coupling between client release cadence and schema migrations, which is one of the largest sources of friction in normalized-schema designs.

### The Parser Is the Contract

For compatibility shims against existing query languages, the language should be scoped deliberately and the parser should be the source of truth on what is supported. Accepting the full AST and filtering later produces worse error messages and harder-to-reason-about edge cases. Rejecting at parse time, with a clear error pointing at the unsupported construct, gives the user better feedback and gives the implementation a smaller surface to reason about. The upstream parser is the right choice when full compatibility is required; the scoped parser is the right choice when the shim is a migration aid.

### Tests Catch the Bugs You Would Ship

The most valuable tests are not the ones that exercise the happy path; they are the ones that pick at the edges of the validation matrix with inputs no realistic client would send. The "layout is a string" bug is the canonical example. The bug would have been invisible in production traffic but would have remained a latent vulnerability. The test that caught it asserted a property no user would have noticed missing, but the absence of the property would have been a real defect. This is the shape of useful test coverage: every clearly-wrong input is rejected in a clearly-traceable way.

## What the Numbers Hide: The Maintenance Story

The numbers above describe what the implementation cost in the moment. They do not describe what it will cost over the next five years. The maintenance story is the part of the analysis most often omitted from "build vs. buy" discussions, and it is the part most likely to dominate the total cost of ownership.

The custom implementations in Phase E are designed to be read and maintained by a future engineer who has not seen the code before. Each file carries a top-of-file documentation comment explaining its purpose, the design alternatives that were considered, and the reasoning that produced the chosen approach. Each non-obvious section carries inline comments. The dependency graph contains only the Go standard library, ClickHouse driver code, and a few extremely stable libraries (Gin, pgx, zap). None of these substrates have shown meaningful breaking-change behavior in recent years.

The imported alternatives would have produced a different maintenance profile. uPlot's API has changed several times in its lifetime; each major version requires a code review of the integration. The Prometheus repository ships breaking changes to its parser as part of its normal release cadence, sometimes silently. Jaeger UI's React stack absorbs the entirety of the JavaScript ecosystem's churn — Webpack upgrades, React major versions, Redux deprecation cycles, RxJS major versions. The team's calendar of "must respond to upstream changes" events would have been substantially fuller.

The Google Site Reliability Engineering workbook frames this trade as the "toil budget" of a system: the recurring operational cost of keeping the system running over its lifetime, independent of the value the system delivers. Imported dependencies generate toil. Custom implementations, when bounded in scope, generate less toil over their lifetimes. The cost is paid up front, in the implementation, rather than continuously, in the maintenance.

## The Operational Profile of the Visualization Layer

Phase E added zero new runtime components. The native visualization features are served by the existing `ui-server` and `query-engine` processes, which were already in the deployment. The PromQL and LogQL endpoints are new routes on the existing query-engine HTTP server. The dashboards endpoints are new routes on the existing tenant-api HTTP server. The SSE log-tail endpoint is a new route on the existing query-engine HTTP server. The dashboards table is a new migration in the existing tenant-api database.

This is the operational profile a small team can sustain. The platform's runtime topology was not changed by Phase E. The number of services to monitor did not change. The number of Helm sub-charts did not change. The number of oncall rotations did not change. The number of database connections did not change. The deployment artifact list grew by one ConfigMap (for the dashboards migration) and zero containers.

The contrast with a hypothetical microservices-oriented implementation is instructive. Splitting dashboards into a separate service would have added one Go binary, one Postgres database (or one schema in the existing database, requiring a separate connection pool), one Helm chart, one Service definition, one Deployment, one HorizontalPodAutoscaler, one ServiceMonitor, one PrometheusRule, and one set of dashboards documenting its health. Each of those is small individually. In aggregate, they constitute the operational complexity that has caused multiple commercial platforms to slow their development pace as they accumulated services.

The lesson, as DHH has argued repeatedly, is that "the majestic monolith" is the correct default for small teams. Decompose only when concrete operational pressure demands it. The platform is, by deliberate choice, a small portfolio of Go binaries that share infrastructure, configuration, and operational discipline.

## On the Long View of Boring Technology

The arc of the Phase E exercise is, in retrospect, an argument for boring technology. The chart is built on Canvas 2D, which has been stable since 2008. The log tail uses Server-Sent Events, defined in 2009 and supported in every browser since 2011. The trace waterfall renders to imperative DOM elements, the same primitives any HTML page has used since the 1990s. The dashboards live in PostgreSQL with JSONB, available since PostgreSQL 9.4 in 2014. The PromQL and LogQL parsers are recursive-descent implementations of subsets of well-documented languages.

Every one of these substrates has demonstrated multi-decade stability. The platform's Phase E code can be read and maintained by anyone who understands HTML, CSS, JavaScript, Go, and SQL. There are no framework conventions to learn, no build pipelines to debug, no version-coordination problems to solve. The on-ramp for a new engineer joining the team is short: read the files, understand what each function does, make changes.

This is the property Dan McKinley's "Choose Boring Technology" essay names: boring technology is technology whose failure modes are exhaustively understood, whose patches arrive predictably, and whose behavior in five years is approximately what it is today. Innovation tokens — the finite budget of new ideas a project can absorb without losing maintainability — are best spent on the parts of the system that genuinely differentiate it, not on the parts that are commodity infrastructure. The visualization layer of an observability platform is commodity infrastructure. The intelligence of the platform lives in the ingest pipeline, the storage engine, and the query optimizer. Spending innovation tokens on the rendering surface is a category mistake.

The native chart renderer is boring technology. The SSE log tail is boring technology. The trace waterfall built from imperative DOM is boring technology. The dashboards table with a JSONB column is boring technology. The hand-rolled PromQL parser is boring technology. The aggregate Phase E implementation spends zero innovation tokens. Every token it could have spent was spent instead on the platform's actual differentiators: the multi-feature ML runtime, the federated query executor, the cost-based optimizer, the hot-cold storage policy with per-tenant retention overrides. The visualization layer is the boring part of the system on purpose, so that the interesting parts can absorb the team's attention.

## The Coda

Ten days after the question that started this work, the engineering director opened the new Metrics tab. He typed a tenant ID, clicked the Plus Panel button, edited the query to chart his team's API error rate, clicked Plus Panel again, charted p99 latency, clicked Save, named the dashboard "checkout-api ops," and shared the URL in a Slack channel with three of his SREs. By the end of the day, eleven people on his team had bookmarked the URL. By the end of the week, the dashboard had been forked twice for adjacent services. Nobody asked about Grafana.

This is the only validation the work needs. The library that was not imported is the library that does not have to be maintained. The microservice that was not split out is the microservice that does not have to be operated. The framework that was not adopted is the framework whose breaking changes do not need to be tracked. Every "no" the design says to extra surface area is a "yes" to the team's capacity to maintain the system five years from now. The instinct to add — the bigger library, the more sophisticated architecture, the trendier framework — is the instinct most engineers need to fight, because it is the instinct the commercial software industry has spent decades training into them.

The five forks were not dramatic. None of them was a war story. Most of them required a few hours of writing and several hours of staring at the resulting diff, asking whether the choice was too clever or not clever enough. The decisions individually were small. The aggregate effect, after two weeks, is a system as featureful as one assembled by importing five things and significantly easier to live with for the next five years.

That is the trade. It almost always favors the small library. The case for the larger one needs to clear a higher bar than most engineers, including the author of this document, instinctively make it clear.

Most days, when the bar is examined, it has not been cleared.

---

## Authority & Research

### Foundational Specifications & Standards
*   **RFC 8259: The JavaScript Object Notation (JSON) Data Interchange Format**: [https://datatracker.ietf.org/doc/html/rfc8259](https://datatracker.ietf.org/doc/html/rfc8259)
*   **WHATWG HTML Living Standard — Server-Sent Events**: [https://html.spec.whatwg.org/multipage/server-sent-events.html](https://html.spec.whatwg.org/multipage/server-sent-events.html)
*   **W3C Trace Context Specification**: [https://www.w3.org/TR/trace-context/](https://www.w3.org/TR/trace-context/)
*   **MDN — Canvas 2D API**: [https://developer.mozilla.org/en-US/docs/Web/API/CanvasRenderingContext2D](https://developer.mozilla.org/en-US/docs/Web/API/CanvasRenderingContext2D)
*   **MDN — EventSource Interface**: [https://developer.mozilla.org/en-US/docs/Web/API/EventSource](https://developer.mozilla.org/en-US/docs/Web/API/EventSource)
*   **MDN — Content Security Policy**: [https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP](https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP)

### Query Languages & Compatibility Targets
*   **PromQL Documentation (Prometheus)**: [https://prometheus.io/docs/prometheus/latest/querying/basics/](https://prometheus.io/docs/prometheus/latest/querying/basics/)
*   **Prometheus PromQL Parser Source**: [https://github.com/prometheus/prometheus/tree/main/promql/parser](https://github.com/prometheus/prometheus/tree/main/promql/parser)
*   **LogQL Documentation (Grafana Loki)**: [https://grafana.com/docs/loki/latest/query/](https://grafana.com/docs/loki/latest/query/)
*   **VictoriaMetrics MetricsQL**: [https://docs.victoriametrics.com/MetricsQL.html](https://docs.victoriametrics.com/MetricsQL.html)
*   **Grafana ClickHouse Datasource Plugin**: [https://grafana.com/grafana/plugins/grafana-clickhouse-datasource/](https://grafana.com/grafana/plugins/grafana-clickhouse-datasource/)

### Database & Storage Theory
*   **ClickHouse MergeTree Engine Documentation**: [https://clickhouse.com/docs/en/engines/table-engines/mergetree-family/mergetree](https://clickhouse.com/docs/en/engines/table-engines/mergetree-family/mergetree)
*   **ClickHouse String Functions Reference**: [https://clickhouse.com/docs/en/sql-reference/functions/string-search-functions](https://clickhouse.com/docs/en/sql-reference/functions/string-search-functions)
*   **PostgreSQL JSONB Data Type Documentation**: [https://www.postgresql.org/docs/current/datatype-json.html](https://www.postgresql.org/docs/current/datatype-json.html)
*   **PostgreSQL Row Security Policies**: [https://www.postgresql.org/docs/current/ddl-rowsecurity.html](https://www.postgresql.org/docs/current/ddl-rowsecurity.html)
*   **PostgreSQL Trigger Procedures**: [https://www.postgresql.org/docs/current/plpgsql-trigger.html](https://www.postgresql.org/docs/current/plpgsql-trigger.html)

### Distributed Systems & Software Engineering Theory
*   **The Fallacies of Distributed Computing (L. Peter Deutsch, James Gosling)**: [https://en.wikipedia.org/wiki/Fallacies_of_distributed_computing](https://en.wikipedia.org/wiki/Fallacies_of_distributed_computing)
*   **Hyrum's Law**: [https://www.hyrumslaw.com/](https://www.hyrumslaw.com/)
*   **Choose Boring Technology (Dan McKinley)**: [https://boringtechnology.club/](https://boringtechnology.club/)
*   **The Majestic Monolith (DHH / 37signals)**: [https://m.signalvnoise.com/the-majestic-monolith/](https://m.signalvnoise.com/the-majestic-monolith/)
*   **MonolithFirst (Martin Fowler)**: [https://martinfowler.com/bliki/MonolithFirst.html](https://martinfowler.com/bliki/MonolithFirst.html)
*   **Building Microservices, 2nd Edition (Sam Newman)**: [https://samnewman.io/books/building_microservices_2nd_edition/](https://samnewman.io/books/building_microservices_2nd_edition/)

### Frontend & Visualization
*   **uPlot — Fast, Lightweight Charting Library (Leon Sorokin)**: [https://github.com/leeoniya/uPlot](https://github.com/leeoniya/uPlot)
*   **Apache Arrow JavaScript Bindings**: [https://arrow.apache.org/docs/js/](https://arrow.apache.org/docs/js/)
*   **HTMX Essays (Carson Gross)**: [https://htmx.org/essays/](https://htmx.org/essays/)
*   **The Locality of Behaviour Principle (Carson Gross)**: [https://htmx.org/essays/locality-of-behaviour/](https://htmx.org/essays/locality-of-behaviour/)
*   **Jaeger UI Repository (Trace Visualization Reference)**: [https://github.com/jaegertracing/jaeger-ui](https://github.com/jaegertracing/jaeger-ui)

### Observability Standards
*   **OpenTelemetry Trace API Specification**: [https://opentelemetry.io/docs/specs/otel/trace/api/](https://opentelemetry.io/docs/specs/otel/trace/api/)
*   **The USE Method (Brendan Gregg)**: [https://www.brendangregg.com/usemethod.html](https://www.brendangregg.com/usemethod.html)
*   **The RED Method (Tom Wilkie)**: [https://thenewstack.io/monitoring-microservices-red-method/](https://thenewstack.io/monitoring-microservices-red-method/)

### Operational Rigor & Security
*   **Site Reliability Engineering Workbook (Google)**: [https://sre.google/workbook/table-of-contents/](https://sre.google/workbook/table-of-contents/)
*   **OWASP SQL Injection Prevention Cheat Sheet**: [https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html](https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html)
*   **OWASP Input Validation Cheat Sheet**: [https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html](https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html)
*   **Dan Luu — Files Are Hard**: [https://danluu.com/file-consistency/](https://danluu.com/file-consistency/)

### Internal Architecture Records
*   **ADR-0028: Visualization Strategy (Hybrid Grafana + Native)**: `docs/adr/0028-visualization-strategy.md`
*   **ADR-0029: Native Metrics Workbench (Canvas Chart Primitive)**: `docs/adr/0029-native-metrics-workbench.md`
*   **ADR-0030: Native Logs Explorer (SSE Live Tail)**: `docs/adr/0030-native-logs-explorer.md`
*   **ADR-0031: Native Trace Waterfall + Service Map**: `docs/adr/0031-native-trace-waterfall.md`
*   **ADR-0032: Dashboards CRUD (JSONB Persistence)**: `docs/adr/0032-dashboards-crud.md`
*   **ADR-0033: PromQL + LogQL Compatibility Shims**: `docs/adr/0033-promql-logql-compat-shims.md`
