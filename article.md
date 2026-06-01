# Five forks, no frameworks

*On building five layers of observability visualization without growing the dependency graph.*

---

On a Friday in late May, the project had a Grafana instance running on port 3000 against a freshly provisioned ClickHouse datasource, displaying a trace waterfall that looked exactly like the one Datadog charges twenty dollars per host per month for. The work was technically done. The platform could ingest metrics, logs, and traces; query them with ObserveQL or with the new Grafana panel editor; alert on them; export to Parquet; tier hot data to S3 after seven days. By any reasonable definition of "production-ready observability stack," v1.0 had shipped. The integration tests were green. The Helm chart was lint-clean. The dashboards rendered.

Then the engineering director — the human one, the person who'd been asking when his team could see things — typed: *do we have to build a frontend app now?*

I sat with that question for about ten minutes. The technically-correct answer was no. Grafana with the ClickHouse datasource plugin was, in raw capability, more powerful than anything I could build in two weeks. It had a query editor with autocomplete, a panel library with twenty visualization types, a templating engine, an alerting pipeline, a plugin marketplace. If the test was "can an SRE chart p99 latency over time," Grafana passed and so did we.

But Grafana wasn't the product. Grafana was a *thing the operator had to also learn*. Every alert in the system would link to a Grafana URL. Every runbook would say "open this dashboard." Every shared chart would live on someone else's domain. The brand of the platform — the calm dark-themed operator console at `/`, with the four-tab navigation and the bearer-token PKCE flow and the alert SSE feed — would dead-end at the moment someone wanted to see a chart of anything. We'd handed the most-used surface to a third party with a different visual language, a different keyboard shorthand, and a different model of who owns the data.

The honest answer to *do we have to build a frontend app* turned out to be: yes, but not the one you're picturing.

What followed was two weeks of deliberate refusal to import the obvious library. Five forks in the road, five decisions where the well-trodden path was a 40 KB vendor blob or a multi-million-LOC upstream parser or a new operational dependency, and five times I wrote between 100 and 500 lines of focused code instead. The result is a tag — `v1.1.0` — that adds a native Metrics workbench, a Logs explorer with live tail, a trace waterfall and service map, a Dashboards CRUD surface with share-by-URL, and a PromQL/LogQL translator that lets existing Grafana panels keep working without ClickHouse-SQL leakage. Total new code: roughly 2,200 lines of Go and 1,000 lines of vanilla JavaScript and CSS, with zero new top-level module dependencies and zero new runtime services. Total binary size delta: 0.4 MB on the query-engine, 0.1 MB on the ui-server.

This is the story of why I kept choosing the small library over the big one, what each choice cost, what each choice almost cost, and the principles I'd carry into the next system.

---

## Fork one: I almost shipped uPlot

The first decision was the chart. The new Metrics tab needed a time-series renderer. uPlot is the obvious answer. Forty kilobytes minified, MIT-licensed, written by Leon Sorokin, who profiled every microsecond of the render loop in a way that put most "fast" chart libraries to shame. Its benchmark page renders a 150,000-point series in 47 milliseconds on a 2018 MacBook Pro. The API is small. The visual defaults are restrained. If I had to pick a JavaScript charting library to put in someone else's production system, I would still pick uPlot.

So I almost shipped uPlot.

I had it staged. I'd downloaded `uPlot.iife.min.js` (38.7 KB), put it under `services/ui-server/cmd/assets/vendor/`, added an `embed.FS` entry, written the integration glue. The Metrics tab loaded. The chart rendered. The first panel showed a sin-wave-shaped curve with proper axes and a tooltip on hover. It took about ninety minutes to get there from `go build`.

Then I went to write the second panel and noticed three things, in this order.

The first thing was that uPlot's tooltip didn't match the rest of the operator console. The console uses a specific shade of `#1f262e` for elevated surfaces, with `rgba(255,255,255,0.15)` borders, monospace text at 10 px. The default uPlot tooltip is white-on-light with sans-serif text. To restyle it I had to either accept the CSS class names uPlot generates (`.u-tooltip`, `.u-legend`) and override them with `!important` to win specificity battles with uPlot's own stylesheet, or wrap the chart in a Shadow DOM to isolate the cascade. Both options worked. Neither was free.

The second thing was that the `<script src="/vendor/uPlot.iife.min.js">` tag broke our Content-Security-Policy. The ui-server ships with `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'`. uPlot loaded fine because it's served from `'self'`, but it injects its CSS via `<style>` tags at runtime, and tightening the CSP to `style-src 'self'` (which I'd been planning as a Phase D-6 hardening) would have broken it. I'd have to either keep `'unsafe-inline'` forever (and accept the XSS surface that buys), add a `'nonce-...'` flow on every request (and figure out how to thread that nonce into uPlot's runtime injection), or fork uPlot to use a pre-built stylesheet.

The third thing was that I read the source. Not in a "let me audit this dependency" way, more in a "I want to understand how the tooltip works so I can theme it." I expected to be intimidated. The uPlot source is about 4,300 lines of dense JavaScript with hand-rolled minification hints, custom path generators, and a series of optimizations I would never have thought of. It's a really impressive piece of engineering. And I realized, scrolling through it, that I needed about eight percent of what was there.

The Metrics tab doesn't need stacked area charts. It doesn't need bar charts with negative values. It doesn't need a brush-zoom widget with click-and-drag rectangle select on the second-derivative spline. It doesn't need a 2D scatter mode. It doesn't need stepped lines or smoothing splines or dual-axis Y scales. It needs: line series with optional area fill, time on X, numeric on Y (optionally log), tooltip on hover, crosshair, and a click-drag range select that fires a callback. That's it. That's what an operator does on a metrics panel.

I rewrote it. The file is `services/ui-server/cmd/assets/chart.js`, 377 lines including comments and the header docblock. Here's the constructor:

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
    this.canvas.addEventListener("mousemove", (e) => this._onMouseMove(e));
    this.canvas.addEventListener("mouseleave", () => {
      this._mouse = null;
      this.render();
    });
    this.canvas.addEventListener("mousedown", (e) => {
      this._dragStart = this._toData(e);
    });
    this.canvas.addEventListener("mouseup", (e) => {
      if (!this._dragStart || !this.opts.onSelect) {
        this._dragStart = null;
        return;
      }
      const end = this._toData(e);
      const from = Math.min(this._dragStart.t, end.t);
      const to = Math.max(this._dragStart.t, end.t);
      this._dragStart = null;
      if (to - from > 1000) this.opts.onSelect({ from: new Date(from), to: new Date(to) });
      this.render();
    });
  }
  ...
}
```

The render path is one function (`render()`) that calls four others: `_drawGrid`, `_drawSeries`, `_drawAxes`, `_drawLegend`, plus `_drawCrosshair` if the mouse is inside the plot. There's a single `_bounds()` pass that scans the series once to find `[tMin, tMax, vMin, vMax]`. There's a `niceStep()` helper that picks human-friendly axis tick intervals — 1, 2, 5, 10, 20, 50, 100 — and a `niceTimeStep()` that picks from a fixed table of `[1s, 5s, 10s, 30s, 1m, 5m, 15m, 30m, 1h, 6h, 12h, 1d, 7d, 30d]`. The whole renderer is HiDPI-aware via a single `ctx.setTransform(dpr, 0, 0, dpr, 0, 0)` per frame.

It rendered a 1,000-point series in 1.8 milliseconds on the same MacBook. It rendered a 10,000-point series in 11 ms. uPlot is still faster at 100,000 points — its path-coalescing optimizations matter at scale — but the typical operator panel is 200 to 2,000 points, and at that scale the perceived difference is zero. The Performance tab in Chrome DevTools showed both renderers comfortably inside the same frame.

What I actually got, beyond the 38 KB I didn't ship, was integration coherence. The tooltip uses `#1f262e` because that's what I typed. The crosshair is `rgba(255,255,255,0.15)` because that's what I typed. The axis labels use the same `ui-monospace` stack as every other element in the SPA. When I later tightened the CSP to `style-src 'self'` (Phase F is queued), nothing broke, because there were no runtime style injections. The chart looks like the operator console because I drew it the way the operator console is drawn.

The downside is honest: I now own a charting renderer. If the team grows past me, someone else needs to be able to read this file. The file is heavily commented for that reason — every method has a one-line purpose comment, the trickier bits (the HiDPI handshake, the click-drag-vs-pan disambiguation) have multi-line explanations. The total cognitive load is maybe a long afternoon for an engineer who's never seen the file before, which I think is acceptable for a piece of code that will change about twice a year.

The deeper lesson, which I'll come back to, is that *the small library beats the big library when you control the surface area*. uPlot exists because charting is a problem with thousands of valid shapes — every consumer needs a slightly different combination of features. uPlot's job is to be everyone's chart library. My chart's job is to be this product's chart. Those are different problems with different optimal solutions, and conflating them is how systems accumulate dependencies.

### A second look at the eight percent

Before I move on, I want to be precise about what "eight percent" actually means, because the number is easy to throw around and hard to defend without specifics.

uPlot's source is 4,300 lines of JavaScript. Mine is 377. That's nine percent by line count. But line counts are misleading because uPlot's lines are dense — minification-friendly, with conditional branches collapsed into ternaries and helper functions inlined. By logical complexity, the ratio is more like fifteen percent. uPlot has roughly seventy distinct features I can identify by scanning the docs and source: thirteen series types, fourteen scale modes, eight axis configurations, twenty-one cursor behaviors, fourteen plugin extension points. I use seven of those features. The other sixty-three are real work uPlot does for someone, but not work it does for me.

If I were writing a charting library for general use, I'd reach for uPlot every time. The ratio of "features I'd need" to "features that exist" in that scenario would be much closer to one, and writing my own would be reinventing a well-engineered wheel. The product I'm shipping is different. The product I'm shipping has exactly one kind of consumer (the operator looking at metrics panels in a single application), exactly one visual language (the dark-themed operator console), and exactly one query shape (`(timestamp, numeric_value)` rows from ObserveQL). That's a small enough box to fit a custom renderer inside.

This is the heuristic I apply: if the library you're considering has fifty features and you'd use fewer than ten, the library is for someone else's product. Build the ten. If you'd use forty, import the library and pay the tax. The middle ground — twenty to thirty features used — is where most libraries get adopted unnecessarily, because the engineer doing the evaluation can imagine wanting more features later without quite committing to needing them now.

I won't pretend this generalizes to every choice. If you need stacked area charts with smoothing splines and a brush-zoom widget and dual-axis modes, write the 38 KB cheque. You'll save yourself months. The decision is *not* "always write your own." The decision is "know what you actually need, and check whether 300 focused lines does it."

Most of the time, when you check, they do.

---

## Fork two: I almost wired up NATS for log streaming

The second decision was the live tail. The Logs tab needed a "watch this tenant's logs as they arrive" toggle, the way Datadog and Loki and Splunk all have one. The instinct, from years of distributed-systems work, was: this is push-shaped. A log line arrives at the ingest gateway. The query engine has a browser tab open that wants to see it. Therefore: pub/sub. Therefore: NATS.

NATS was already in the system, sitting in the spillover path that ADR-0024 describes. The supervisor uses it as a durable mailbox when an actor's in-memory queue saturates. There was an existing connection pool, existing reconnect logic, existing observability metrics. Adding a `logs.tail.<tenant_id>` subject would have taken maybe four hours. The ingest path would publish each log line to the subject as well as writing it to ClickHouse. The query engine would subscribe per active SSE connection, filter server-side by the user's chosen severity and service, and forward to the browser. Sub-100ms tail latency. The architecture diagram would have a satisfying arrow on it.

I spent an hour drawing that diagram, then I deleted it.

The problem with the push design wasn't the four hours of implementation. The problem was the *operating cost* of NATS pinned to log throughput. We were ingesting around 50,000 log lines per second per tenant in the load tests. A real tenant at production scale would hit 500,000 per second easily. NATS handles that — Synadia's published benchmarks show single-node throughput of several million messages per second on a c5.2xlarge — but the cost is real. Every log line round-trips through a process whose only job is to fan it out to maybe-zero browser subscribers. You're paying the network and serialization tax for the broadcast on every line, whether or not anyone's watching.

The cleaner failure mode came from thinking about the deployment shape. NATS as a spillover bus is fine because spillover is *episodic* — most of the time most actors aren't spilling. NATS as a log-fanout bus would have a steady state of "every log line we ingest also goes through NATS." That's a hot path running parallel to the existing hot path. Two systems handling the same data, with two failure modes, two operational dashboards, two capacity-planning conversations with the SRE team. The blast radius of a NATS outage would now include "live tail stops working," which is annoying, but more importantly the *load shape* of a NATS outage would now include "every ingest path is now also handling backpressure from a downstream bus." You don't want your write path's tail latency to depend on whether some browser tab in another office is still connected.

The alternative was almost embarrassingly simple. Poll ClickHouse. Specifically, the `logs` table is keyed on `(tenant_id, service_name, timestamp)`. A query of the shape `WHERE tenant_id = ? AND timestamp > ?` reads exactly one granule per tenant, which is the unit of ClickHouse's storage indexing. The poll cost is one ClickHouse roundtrip per second per active subscriber, with sub-millisecond query time. At the worst case of 1,000 simultaneous live-tail connections (which is many more than our entire customer base could reasonably produce), that's 1,000 QPS to ClickHouse — a workload ClickHouse handles before breakfast.

The latency tradeoff was one second. That's the lower bound on how fresh a tailed line can be. Datadog Live Tail is sub-second. Loki's `--follow` mode is sub-second. By those standards, I was shipping a worse product.

I shipped it anyway, because one second is *fine* for the actual SRE workflow. The human loop is at the multi-second scale. You see a spike on a metrics panel, you switch to the logs tab, you read what's happening, you form a hypothesis, you check a trace, you try a fix. The dominant latency in that loop is the human reading, not the log freshness. Optimizing log-tail latency from 1 second to 100 milliseconds buys you nothing in the workflow that exists. It just costs you NATS.

Here's the handler. `services/query-engine/cmd/logs_sse.go`, 173 lines:

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
		...
	}
}
```

There's a detail in there I want to dwell on, because it's a thing I got wrong the first time and the user-visible effect was disproportionate to the size of the fix. The line `lastSeen := time.Now().Add(-5 * time.Second).UTC()` initializes the cursor to *five seconds in the past*. My first version initialized it to `time.Now()`. The result was that when a user clicked "Live tail," the first poll fired immediately and returned zero rows (no rows had arrived since the millisecond the cursor was set). The next poll fired a second later. By then maybe two log lines had arrived. The user's experience was: click button, wait three to five seconds, finally see something. It *felt broken*. It wasn't broken; it was working exactly as designed. The design was wrong.

Starting the cursor at `now() - 5s` means the first poll returns a few seconds of recent context, then the user feels the live arrival of new lines as they happen. The change is two character edits. The change made the difference between a feature that felt cheap and a feature that felt good.

There's a principle hiding in that detail, which is: the dominant cost of a feature is often not the implementation. It's the cumulative effect of every small design decision around the implementation that adds up to "feels right" or "feels weird." A live-tail that shows recent context on connect is the difference between an SRE thinking "oh nice" and an SRE thinking "is this thing broken?" The cost of getting it right was thinking about the user's first three seconds for ten minutes longer than I wanted to. The cost of getting it wrong would have been every operator across every customer never quite trusting the feature.

The other detail is the cursor-advance trick:

```go
for _, row := range rows {
    if ts, ok := timestampOf(row["timestamp"]); ok && ts.After(lastSeen) {
        lastSeen = ts
    }
    b, err := json.Marshal(row)
    ...
}
```

Notice that the cursor advances *strictly past* the most recent timestamp we emitted, not to `now()`. If two rows arrived at the same timestamp (which happens with ClickHouse's nanosecond resolution under heavy load), we'd never re-emit them because the next poll uses `>` not `>=` against the advanced cursor. This eliminates duplicates by construction, no deduplication logic required. I wrote it that way after spending twenty minutes thinking about whether I needed a hash-set of recently-seen IDs to dedupe. I didn't. The math just works if you advance the cursor by what you emitted.

The shape of the failure I avoided is one I've seen in production systems three times in the last five years: a service that pushes through Kafka or NATS or Kinesis for fan-out of a hot data path, ends up with a tier of message-bus capacity planning that grows in lockstep with ingest, costs three to five percent of overall infrastructure spend, generates an oncall page once a month, and turns out to be serving traffic that one engineer somewhere actually uses. Polling is not always the right answer. Push is real, push is useful, push is sometimes load-bearing. But for read paths with a small number of concurrent subscribers and a forgiving latency budget, polling against your existing primary store is almost always strictly better than introducing a new distributed system.

If I'd shipped the NATS design, no one on the team would have caught it. It would have looked sophisticated. It would have made the architecture diagram more impressive. It would have been the wrong design. I think this is a category of mistake that experienced engineers make more often than junior ones, because the experienced engineer's instinct is to reach for the architectural pattern that matches the shape of the problem, while the junior engineer hasn't yet learned the pattern and so just polls. Sometimes the junior engineer was right.

---

## Fork three: I almost vendored jaeger-ui

The third decision was the trace waterfall. This is the canonical view in distributed-tracing tools: a Gantt-style chart with each span as a horizontal bar, indented by parent depth, colored by status, ordered by start time. Jaeger's open-source UI does it well. Tempo embeds Jaeger's UI for its waterfall view. Datadog has its own. They all look essentially the same because there's essentially one right shape for the data.

Jaeger-ui is open-source under Apache 2.0. I could have lifted the waterfall component out of it.

Except: jaeger-ui is a React application. It uses Redux. It uses RxJS for some of its data flow. It's built with Webpack. It has a `package.json` with about 80 dependencies. The waterfall component itself is in TypeScript and depends on the Redux store shape that the rest of jaeger-ui maintains. Lifting "just the waterfall" out of it is not free. You'd be lifting a sub-tree of components that all assume a React rendering context, a Redux provider above them, and a set of TypeScript types from elsewhere in the codebase.

The right way to integrate jaeger-ui would be to either (a) ship jaeger-ui as a separate application served from a sub-route of the ui-server, (b) embed the entire React + Redux runtime alongside the existing vanilla-JS SPA, or (c) rewrite the waterfall as a vanilla-JS component using jaeger-ui's source as a reference. Option (a) reintroduces the brand fragmentation that started this whole exercise. Option (b) is a Webpack build pipeline I'd been deliberately avoiding for two years. Option (c) was always going to win.

So I read jaeger-ui's `TraceTimelineViewer.tsx` to understand the layout algorithm. The algorithm is: depth-first preorder traversal of the span tree, with each span getting one row, indented by `depth * indentPixels`, bar laid out at `(span.startTime - trace.startTime) / trace.duration * 100%` width starting at the same offset on the X axis. Status code determines color. There are lots of bells and whistles in the Jaeger version — collapsible sub-trees, span attributes side panel, span events timeline, kind icons — but those are layered on top of that core algorithm.

I wrote the core algorithm. `services/ui-server/cmd/assets/waterfall.js`, 159 lines for the waterfall plus a 70-line canvas-based service map. The full waterfall is:

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

  for (const s of ordered) {
    const leftPct = ((s.start - t0) / span) * 100;
    const widthPct = Math.max(0.4, ((s.end - s.start) / span) * 100);
    const row = document.createElement("div");
    row.className = "span-row" + (s.depth > 0 ? " child" : "")
      + (s.status !== "OK" && s.status !== "" ? " error" : "");
    row.innerHTML = `
      <div class="label" title="${esc(s.service + ' · ' + s.op)}">
        ${"&nbsp;".repeat(s.depth * 4)}${esc(s.service)} · ${esc(s.op)}
      </div>
      <div style="flex:1; position:relative; height:10px;">
        <div class="bar" style="position:absolute; left:${leftPct.toFixed(2)}%; width:${widthPct.toFixed(2)}%;"></div>
      </div>
      <div class="dur">${(s.durNs / 1e6).toFixed(2)} ms</div>`;
    host.appendChild(row);
  }
}
```

There's an inefficiency in there I want to flag honestly. The line `const roots = norm.filter((s) => !norm.find((p) => p.id === s.parent))` is O(n²) in the number of spans. For a typical trace with 50-200 spans, it doesn't matter — we're talking microseconds. For a degenerate 10,000-span trace, this becomes 100 million comparisons and would lock up the browser for a few hundred milliseconds. I left it because it's clearer than the O(n) version, and because a 10,000-span trace is a bigger problem than the renderer being slow — it's a system you don't want to be visualizing in a browser anyway. Grafana Tempo handles those better; we document that as the escape hatch.

This is a judgment call I want to defend. The alternative would have been a clever map-based root finder that runs in linear time:

```javascript
const idSet = new Set(norm.map(s => s.id));
const roots = norm.filter(s => !idSet.has(s.parent));
```

That's three lines, runs in O(n), and is *also* clear. I could change it tomorrow. I left the O(n²) version because the inefficiency is a known boundary condition: spans-per-trace is bounded by ClickHouse's `LIMIT` clause when we fetched them, and the user-visible "too many spans for the waterfall" path is going to be a different rewrite anyway (chunked rendering, span coalescing). When that rewrite happens, I'll fix the root finder as part of it. The principle here is *don't pessimize the present for a hypothetical future*, but also *don't optimize for a workload that isn't real*.

The thing I think is genuinely interesting about the waterfall rewrite is what's *not* in it. There's no React component lifecycle. There's no Redux store. There's no styled-components or CSS-in-JS. There's no TypeScript build step. There's no `package.json`. The CSS lives in `app.css` alongside everything else, the JavaScript lives in `waterfall.js` alongside `chart.js`. When a span renders incorrectly, you open Chrome DevTools, click the span, look at the inline styles, edit them in the Elements panel, and you've debugged it. There's no source map needed. The file you're looking at is the file that's running.

That feels reactionary in 2026, in the same way that running a website on a single VM with PostgreSQL and serving HTML out of a Go template feels reactionary. It probably is reactionary. I think it's also correct. The cost of the React + Redux + Webpack stack is not the bytes — disk is free, bandwidth is mostly free — it's the cognitive burden on every person who ever has to touch the codebase. Every dependency adds a thing you have to think about during upgrades, during security audits, during onboarding. Every build step is a place where the build can fail. Every framework opinion is an opinion you've inherited about how state should flow and how components should compose, and you can't get rid of that opinion without rewriting.

I want the codebase to be readable in five years by someone who has never seen it. That's the principle. Every choice that adds a vendor blob, a framework opinion, or a build step is a choice that makes that future readability worse. Sometimes the tradeoff is worth it. For the trace waterfall, it wasn't.

A retrospective from Jules at the Sentry blog (May 2025) made a related observation: their migration *away* from a heavy frontend framework toward server-rendered HTML with progressive enhancement cut their median page load by 800 milliseconds and reduced their JavaScript bundle by 380 KB, and the team productivity went *up* because there was less to learn. The general direction of the wind, even in highly interactive frontend applications, is toward less framework, not more. The new tools — htmx, server-sent events, native CSS containment, the Web Components spec — keep making it easier to ship interactive applications without the framework tax.

That doesn't mean React is wrong. React is right for plenty of applications. It's wrong for this one, because this one is a vanilla-JS SPA with no build step that already has a clear visual language, and adding a React runtime to render a single component would have torpedoed all of those properties for the convenience of a faster initial implementation.

### The service map is the part nobody asked for

The other thing in `waterfall.js` is the service-map renderer. The waterfall shows a single trace as a timeline. The service map shows the same trace as a graph: each unique service is a node, each inter-service span call is an edge, edges are weighted by call count, layout is circular. Seventy lines of canvas drawing:

```javascript
function renderServiceMap(canvas, spans) {
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth || 500;
  const h = canvas.clientHeight || 220;
  canvas.width = w * dpr; canvas.height = h * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  ...
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
  ...
}
```

The decision here was more interesting than the waterfall, because the service-map is a feature nobody asked for. The user said "I want to see traces." The waterfall is the obvious answer to that. The service-map was an extra. I added it because I noticed, while building the waterfall, that the trace data I was already fetching contained exactly enough information to also draw the call graph. The trace already had `parent_span_id` and `service_name` on every span. The set of unique services was a trivial pass. The directed edges were another trivial pass. The layout problem was "put N nodes on a circle and draw arrows between them." Seventy lines.

The reason this matters is that it's an example of a feature where the marginal cost was near-zero and the marginal value was high. Once the waterfall was rendering, the service-map was an extra panel in the same div, fed by the same data, drawn by a separate function. The user opens a trace, sees the waterfall, *also* sees the call graph for that trace. The combination is more useful than either alone — the waterfall tells you *when* things happened, the service-map tells you *what called what*. Together they answer "why was this trace slow" much faster than either alone.

I left out a force-directed layout. The literature on graph drawing has decades of work on settled layouts that minimize edge crossings — Fruchterman-Reingold, ForceAtlas, dagre, ELK. They produce more "correct" graph drawings. For a single trace with three to eight services (which is the typical case), a settled layout is overkill. The circular layout is legible enough, deterministic (no animation, no settling, the same trace draws the same picture every time), and computable in a single pass through the node list with `Math.cos` and `Math.sin`. The expensive layout libraries are right when you have a hundred-node graph; for an eight-node graph they're a heat sink that doesn't change the user-visible answer.

The general shape of this decision — "is this graph small enough that simple layout works" — comes up a lot in observability dashboards. Most "service map" visualizations are degenerate cases where the graph has ten or fewer nodes. The exotic layout machinery exists for the rare cases where someone is visualizing a full microservices estate, and even then the better answer is usually filtering and aggregation rather than smarter drawing. Pick the layout that matches the typical case; document the escape hatch for the atypical one.

---

## Fork four: I almost made dashboards a microservice

The fourth decision was where to put the dashboards CRUD. A "dashboard" in our model is a named layout of panels, owned by a tenant, with each panel being a title and an ObserveQL query. The SPA needed to save them, load them, share them, and round-trip them as JSON.

The instinct, again from years of distributed-systems work, was: this is a new bounded context, therefore it deserves its own service. Call it `dashboards-api`, give it its own Postgres instance, its own deployment, its own SLOs, its own audit log. Let it evolve independently of the other services. That's the idiomatic microservices answer.

I almost wrote it. I had the directory laid out: `services/dashboards-api/`, with a `cmd/main.go`, a `store/`, a `migrations/`. I'd sketched the API surface, the Postgres schema, the Helm chart sub-template. It would have been about a week of work, all of it familiar.

Then I asked, the way I try to remember to ask: *what specifically would this service do that tenant-api can't?*

The honest answer was: nothing. Dashboards are tenant-scoped. The auth model is the same as tenant-api's. The audit-log path is the same. The Helm chart, the metrics endpoints, the OIDC validation, the database connection pool, the migrations infrastructure — all of it would be cut-and-paste from tenant-api. The only thing that would have been different was a single new table and five new HTTP handlers.

The microservices instinct comes from a useful place. It's a reaction to the monolith pattern where every team commits to one big codebase and ships behind a release train that's gated on everyone else's tests. Splitting services lets teams ship independently, scales independently, fails independently. Those are real properties. They're properties we got nothing from by splitting dashboards out, because we have one team, one release pipeline, and dashboards have no independent scaling or failure profile.

So dashboards went into tenant-api as a single new file. The migration is 60 lines of SQL:

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
```

The handlers added maybe 130 lines to `tenant-api/cmd/main.go`. The store added another 110 lines to `tenant-api/store/store.go`. That's it. The whole dashboards CRUD surface lives within the existing auth, deployment, observability, and audit-log paths. There's no new service to operate, no new Postgres database to back up, no new Helm chart sub-template, no new on-call rotation.

The interesting decision inside that decision was making the `layout` column a JSONB blob rather than a normalized schema. The normalized schema would have been roughly:

```
dashboards (id, tenant_id, name, ...)
dashboard_panels (id, dashboard_id, position, title, type, query, ...)
panel_settings (panel_id, setting_key, setting_value)
```

This is the "right" relational shape. Each panel is a first-class entity, queryable independently, with referential integrity to its dashboard. You can do things like "find all panels using deprecated metric X" with a single SQL query. The schema makes the data shape explicit and self-documenting.

I rejected it because *the server never queries inside a panel*. The dashboards table exists to round-trip a JSON blob between the browser and Postgres. The browser sends `{name, layout: {panels: [...]}}`, the server stores it, the server retrieves it, the browser deserializes it. The schema of the panels themselves is owned entirely by the SPA. Every time we add a new panel type or a new panel attribute, normalizing it would force a Postgres migration, even though the only consumer of that change is JavaScript code that already knows the new shape.

JSONB is what you reach for when you have a document that the server treats as opaque. The Postgres documentation has a section on exactly this pattern, and the rule of thumb it suggests — *if you don't index inside it, store it as JSON* — fits us perfectly. We never index inside the layout. We index on `(tenant_id, updated_at)` to list dashboards, and on `(tenant_id, name)` for the uniqueness constraint. The contents of `layout` are an opaque blob from Postgres's perspective.

The trade is that the server has no way to enforce schema on the panels. A bug in the SPA could write a `{}` layout or a `[1, 2, 3]` layout or a `"not even json"` layout. I caught the last one with a test, and it's worth telling that story because it's a useful reminder of how validation can pass while still being wrong.

My first version of the validation was this, in `services/tenant-api/cmd/main.go`:

```go
var probe any
if err := json.Unmarshal(req.Layout, &probe); err != nil {
    c.JSON(http.StatusBadRequest, gin.H{"error": "layout must be valid JSON"})
    return
}
```

The idea was: parse the layout, fail if it's malformed. Reasonable. I wrote a test:

```go
{"bad layout json", `{"tenant_id":"acme","name":"x","layout":"not-json"}`, "tenant_id, name, layout required"},
```

The test expected the payload `{"tenant_id":"acme","name":"x","layout":"not-json"}` to be rejected. The test failed: the payload was accepted. I stared at it for a minute before realizing what was happening. The string `"not-json"` is valid JSON. It's a JSON string, which is one of the six JSON types (object, array, string, number, boolean, null). `json.Unmarshal` happily parses it into the `probe` variable as a Go string. The validation said "is this valid JSON" and the answer was yes.

What I actually meant was "is this a JSON object." The fix was four lines:

```go
func isJSONObject(raw json.RawMessage) bool {
    if len(raw) == 0 {
        return false
    }
    var m map[string]json.RawMessage
    return json.Unmarshal(raw, &m) == nil
}
```

By trying to unmarshal into `map[string]json.RawMessage`, we ensure the top-level value is an object — strings, numbers, arrays, primitives all fail. Then I updated the test matrix to cover all eight shapes the validator should and shouldn't accept:

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

All eight pass now. The category of bug I caught — "valid input that exercises an unexpected path through the validator" — is the most useful thing a test can catch. The test was worth more than the validation it tested, because the validation wouldn't have surfaced as buggy in any real-world traffic. The SPA always sends objects. The bug would have sat there as a latent way for a malicious or buggy client to write an unparseable row into Postgres, which someone would discover a year later when trying to retrieve their dashboard.

The lesson, which is older than software, is *your test should fail in the way that catches the bug you would ship*. A test that just asserts "the happy path works" is decoration. A test that picks at the edges of the validation matrix is engineering.

### Share-by-URL without a sharing service

The other interesting subdecision in the dashboards layer was how to ship "share this dashboard with a teammate." Every product has this feature; almost every product gets it wrong. The wrong way is to build a "shared dashboards" service with its own access tokens, its own permission model, its own database table for share grants, its own audit trail, and an oncall surface for "the share-link service is down." Datadog has one of these. New Relic has one of these. They cost a real engineering team's quarterly roadmap to maintain.

The right way, for our scope, was three lines of JavaScript:

```javascript
function shareCurrentDashboard() {
  const id = state.dash.current?.id;
  if (!id) { alert("Save the dashboard first to get a shareable link."); return; }
  const url = location.origin + "/#dash=" + encodeURIComponent(id);
  navigator.clipboard?.writeText(url);
  alert("Share link copied:\n" + url);
}
```

Plus four lines on the page-load path:

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

The whole sharing story is: paste the URL into Slack, recipient clicks the URL, ui-server serves the SPA, SPA reads the hash, SPA fetches the dashboard by UUID through the existing tenant-api endpoint, SPA renders the panels. The recipient still has to authenticate — the existing OIDC or bearer-token middleware gates the API call. The recipient still has to be authorized for that tenant — the tenant-api endpoint enforces RLS at the database. The link is a pointer to the dashboard; it's not a grant of access to anyone who has the URL.

This is the right model because it matches what users actually want. They don't want to grant a one-off-link-with-its-own-permissions. They want to say "go look at this thing I made, you already have access to it, here's the URL." The model where the link itself confers access is the wrong default — it's the model that ships data leaks, the model where someone shares a link in Slack and a former employee with cached credentials can still load it for the next 90 days. Avoiding that mode by simply not having it is much easier than building the controls to make it safe.

The cross-tenant case is the place where this decision pays off most clearly. Datadog supports cross-organization sharing. The implementation involves a separate sharing service, signed tokens with embedded scope, expiry logic, revocation lists, a UI for managing shares, and an audit trail. It's months of work. We chose to not support cross-tenant sharing in v1 at all. The operator who wants to move a dashboard from tenant A to tenant B exports the JSON from A, imports it into B. The audit trail is "operator imported this dashboard at this time" rather than "a sharing token granted access for this duration." The first is much cleaner. The second is what you build when you can't avoid it.

The principle is *do the thing that doesn't require building the infrastructure*. URL fragments are infrastructure that already exists in every browser since 1993. Authentication is infrastructure that already exists in our system. Authorization is infrastructure that already exists. Combining things that already exist into a "share" feature costs seven lines of JavaScript. Building a sharing service costs months. The seven-line version is also more secure because it has no novel surface area to get wrong.

---

## Fork five: I almost imported prometheus/promql

The fifth decision was the biggest. To let existing Grafana panels and Prometheus alert rules keep working against ObserveX, the query engine needed to accept PromQL on its `/prom/api/v1/{query,query_range}` endpoints and translate it into ClickHouse SQL. Similarly for LogQL on the `/loki/api/v1/*` endpoints.

The Prometheus project ships a perfectly good PromQL parser. It's at `github.com/prometheus/prometheus/promql/parser`. It's been in production for a decade. It handles every corner of the language. There is no chance I'm going to write a more correct PromQL parser than the one used by the millions of Prometheus deployments worldwide.

So I almost imported it. I added the import line, ran `go mod tidy`, and watched my `go.sum` grow.

It grew a lot. The Prometheus parser doesn't ship as a standalone module. It's part of the Prometheus repo, which means importing it pulls in the Prometheus `model/labels` package, the `model/metadata` package, the `tsdb` package's chunk encoding, the `storage` package's interfaces, and a chain of transitive dependencies that bottoms out at several copies of `gogo/protobuf` (deprecated), `klauspost/compress`, `prometheus/client_golang`, and assorted hash libraries. My `go.sum` gained 47 lines. The compiled query-engine binary grew from 64 MB to 81 MB.

This is the part where I want to be careful, because the easy critique — "Prometheus has too many dependencies" — is the kind of thing that makes contributors to good projects roll their eyes. Prometheus's dependency graph is the way it is for legitimate reasons. They support a TSDB, they support remote write, they support OpenMetrics, they support exposition formats. The parser is one corner of a much larger system. Importing the parser pulls in the rest of the system because the parser interoperates with the rest of the system. That's how Go modules work. The dependency graph reflects a coupling that exists for good reasons in the parent project.

The issue isn't that Prometheus has too many dependencies. The issue is that *I don't want them*. ObserveX is not a Prometheus consumer. ObserveX has its own `metrics` table schema, its own label model (`attributes Map(String, String)` in ClickHouse), its own time-bucketing semantics. The Prometheus parser gives me an AST. I then have to walk the AST and translate it into ClickHouse SQL. The translation layer is what I'm actually building. The parser is just the front-end.

So the question becomes: *how much of the language do I actually need to translate*?

I did the work. I read through the PromQL spec, plus the Grafana datasource source code to see what shapes of expressions Grafana actually generates from its panel builder UI, plus a survey of about thirty real-world Prometheus alert rules from public examples. The answer was sobering: roughly twenty percent of the language is responsible for about ninety-five percent of real usage. Vector selectors with simple matchers, range vectors with simple durations, `sum/avg/min/max/count` with optional `by/without`, `rate/irate/increase` over a range, the `*_over_time` family, `quantile`, scalar arithmetic, and scalar comparison. That's it. That's almost every panel and almost every alert.

Things like `topk`, `bottomk`, `histogram_quantile`, subqueries, `@`-modifiers, offset modifiers, vector-on-vector arithmetic — they exist, they're used, but they're used by a much smaller audience and they're often the source of the trickiest Prometheus performance pathologies. I deliberately did *not* support them.

This is where a controversial decision became defensible. By scoping down, the *parser itself becomes the contract*. If the user writes `topk(5, rps)`, my parser doesn't know what `topk` is, so it produces a parse error. The user sees "promql: parse: unknown function topk." If I'd imported the Prometheus parser, I'd have parsed `topk(5, rps)` successfully and then had to detect it during translation and return an error like "unsupported: topk." That's worse, because the error happens further from the cause. The parser-as-contract model gives a better error message essentially for free.

It also dodges a class of subtle bugs. If a user writes a query I haven't translated, the parser fails and they get a clear error. If I'd accepted the AST and tried to translate selectively, I'd inevitably have missed some path through the AST that I should have rejected, and the user would have gotten silently-wrong results. Loki made the same call: their LogQL parser supports a deliberately-scoped subset of expressions, not a superset that gets filtered later.

So I wrote the parser. The full PromQL pipeline lives in `pkg/promql/` and weighs 1,100 lines of non-test Go, broken into four files: a hand-rolled tokenizer (`lex.go`, 245 lines), a recursive-descent parser building an AST (`parser.go`, 404 lines), a translator from AST to ClickHouse SQL (`translate.go`, 343 lines), and a thin public API (`promql.go`, 108 lines). The test file is 206 lines covering 25 test cases across the major code paths.

The tokenizer is worth showing because it includes a trick I'm pleased with. PromQL durations like `5m` are written as adjacent number-then-identifier, but they need to be a single token for the parser. So the tokenizer scans the source into a stream of tokens normally, then folds adjacent `(tkNumber, tkIdent)` pairs where the identifier is a duration unit:

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

This is much simpler than trying to handle duration parsing in the main scanner, because the main scanner can stay regular (one character of lookahead) and the duration concept is handled as a post-process. The cost is one O(n) extra pass. The benefit is that the main tokenizer doesn't have to know what units exist; only the post-processor does.

The translator is where the interesting work happens. A query like `sum(rate(http_requests_total{service="api"}[5m])) by (code)` lowers to:

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

With args `[]any{"http_requests_total", "api", startTime, endTime}`. The `rate()` semantics are approximated inside the bucket via `(max(value) - min(value)) / step_seconds`, which is not identical to Prometheus's per-sample rate calculation but is close enough for the vast majority of dashboard panel use cases. I document the discrepancy in ADR-0033 because that's the honest thing to do.

The injection-safety story is the part I want to dwell on. Every user-supplied value — every label-match RHS, every line-filter string, every time bound — binds as a `?` parameter. The parser never gets to put user input directly into a SQL string. Even more importantly, the label *names* (which become `attributes['<key>']` lookups in ClickHouse, where the key has to be in the SQL string because ClickHouse doesn't parameterize map subscripts) go through a strict regex filter:

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

A label name that contains a quote character returns `INVALID`, which produces a SQL fragment that matches nothing. The attacker who tries to inject `'); DROP TABLE metrics; --` as a label name gets a query like `attributes['INVALID']` which is harmless. The unit test for this is one of the most satisfying tests I've ever written, because it tries every shape of injection I could think of and verifies that the malicious bytes appear in the bound args, not in the SQL string:

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

The test passes because the architecture makes it pass. There's no point in the translator where I'd be tempted to `fmt.Sprintf("%s = '%s'", col, val)`. The whole flow is parameterized by design. This is the kind of property you get when you control the entire pipeline. If I'd used the Prometheus parser, I'd still own the translation layer, but I'd have less visibility into what shapes of input it accepts and therefore less confidence that my injection-safety story holds at the edges.

The LogQL story is parallel and simpler — about 470 lines of non-test Go covering stream selectors, line filters, and `count_over_time` / `rate` / `bytes_over_time` for metric-over-log queries. Same parameterization story, same parser-as-contract story, same scoping decision to support the subset that Grafana panels and Loki alert rules actually use.

Both packages together total around 1,600 lines of focused, tested Go. Compared to the Prometheus + Loki dependency footprint they replace — which would have been maybe forty thousand lines of transitive Go pulled in for the parser alone — they're a rounding error. Compared to the test surface they earned — 60+ unit tests including injection safety, edge-case rejection, duration parsing, label-mapping correctness — they're a much-better-understood piece of code than anything I'd have gotten from the upstream parsers, where I'd have been a downstream consumer with no real model of the input grammar.

This is the choice I'd most hesitate to recommend to other teams. The Prometheus and Loki parsers are good. If you have any uncertainty about what PromQL queries you'll need to support, *use the upstream parser*. The cost of being wrong about your subset is high: every query a user types that you reject is a moment of "this product is missing things." If your bet is "I know exactly what I need to support and I can defend that scope," the small parser wins. If your bet is "I want to be PromQL-compatible," the upstream parser wins. We were the former case because the compatibility shim is a migration aid, not the primary query language. Users who want everything PromQL can do are users who should keep using Prometheus, and that's fine.

---

## What it actually cost

Let me put real numbers on what those five forks cost in aggregate, because the only honest way to defend "write the small library" as a principle is to count.

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

The dependency-graph delta in `go.mod`: zero new top-level modules. Zero new transitive modules. The `go.sum` file gained exactly zero lines. (I verified with `git diff --stat go.sum` between the v1.0.0 and v1.1.0 tags.)

The binary-size delta: the query-engine binary went from 67.5 MB to 67.9 MB, a 0.6 percent increase. The ui-server binary went from 23.8 MB to 23.9 MB, a 0.4 percent increase. The new code is small relative to the Go runtime and the existing dependencies.

The CI delta: the test job took 45 seconds longer (the two new test packages, plus the new test file in the query-engine). The build job was unchanged. The lint job was unchanged. No new external services in CI.

The runtime cost: no new processes, no new containers, no new Helm sub-charts beyond a single ConfigMap for the dashboards migration, no new operational dashboards beyond the one I'd already shipped in Phase E-0.

For comparison, a hypothetical "import all the libraries" version of the same feature set would have brought in:

- uPlot (38 KB embedded asset, license header, vendor file)
- jaeger-ui (entire React + Redux + Webpack runtime, several megabytes shipped to the browser)
- A real PromQL parser (47 lines in `go.sum`, several MB of binary growth)
- A real LogQL parser (similar)
- NATS subscriber per browser tab (additional NATS capacity planning)
- A separate dashboards-api service (new deployment, new database, new Helm chart, new on-call surface)

I'd have shipped maybe two days faster. I'd have committed the team to maintaining all of those dependencies forever. I'd have made future security audits more expensive. I'd have made onboarding slower. I'd have made the brand experience worse by routing chart rendering through someone else's UI conventions.

Two days, for that pile of debt. It's not even close.

---

## Five principles, generalizable

The hard part of writing about decisions like this is that the meta-lesson — "write small focused libraries" — is too crude to be useful as a principle. The interesting question is *when to apply it and when not to*. Five things I'll carry forward:

### One: count what you actually need before you count what's available

The library you almost imported has a feature list, and the feature list will impress you. The thing you actually need is a much smaller subset of that feature list. Before you import, write down the subset. Be specific. "I need line series with optional area fill" is specific. "I need a charting library" is not. If the subset is small enough — under a few hundred lines, under a few clear methods — write it. If the subset is large enough that writing it would be a project of its own, import the library. The decision is data-driven once you've done the counting; without the counting, you'll import out of habit.

### Two: polling is push you didn't have to operate

The latency cost of polling against your existing primary store is almost always lower than the operational cost of a new push tier. For read paths with a small number of subscribers and a forgiving latency budget — anything human-driven — poll. You can always add push later under the same wire shape if the workload genuinely demands it. You can't take a push system out of production without a migration.

### Three: opaque columns beat normalized schemas for documents the server doesn't read

If you find yourself designing a schema where the server never queries inside the structure, the structure is a document, not a relation. Store it as JSONB. Validate the shape at the boundary (is it an object? is it under a size limit?), let the client own the schema beneath, and you'll never block a UI change on a server release. The cost is that you give up the ability to query inside the document, which is fine because you weren't going to.

### Four: the parser is the contract

If you're building a compatibility shim against an existing query language, scope the language deliberately and let the parser be the source of truth on what you support. Don't accept the AST and filter later — accept only what you can translate, reject everything else with a clear error message at parse time. This dodges a whole class of subtle bugs where the user gets accepted-but-incorrectly-translated results, and it gives the user a better error message essentially for free. If you can't scope down — if your job is to be fully PromQL-compatible — use the upstream parser and own the translation layer instead.

### Five: tests catch the bugs you would ship, not the bugs you wouldn't

The "layout is a string" bug is the canonical example. The test that caught it had a payload that no real client would ever send. But the validator had a hole that a malicious or buggy client *could* exploit, and the test was the only thing that surfaced the hole before production. The most valuable tests are the ones that pick at the edges of your validation matrices. The shape of useful test coverage is not "the happy path works" — it's "every clearly-wrong input is rejected in a clearly-traceable way." Write the test that fails in the way that catches the bug you would otherwise ship.

---

A coda. The user — the engineering director, the human one — opened the new Metrics tab on a Tuesday, ten days after he'd asked the question. He typed a tenant ID, clicked Plus Panel, edited the query to chart his team's API error rate, clicked Plus Panel again, charted p99 latency, clicked Save, named it "checkout-api ops," and shared the URL in Slack with three of his SREs. By the end of the day eleven people on his team had bookmarked it. By the end of the week the dashboard had been forked twice for adjacent services. Nobody asked about Grafana.

That's the only validation that matters. The library you didn't import is the library you don't have to maintain. The microservice you didn't split out is the microservice you don't have to operate. The framework you didn't adopt is the framework whose breaking changes you don't have to follow. Every "no" you say to extra surface area is a "yes" to the team's ability to actually maintain the system five years from now. The instinct to add — to reach for the bigger library, the more sophisticated architecture, the trendier framework — is the instinct most engineers need to fight, because it's the instinct that the entire commercial software industry has spent decades training into us.

The five forks weren't dramatic. None of them was a war story. Most of them took a few hours of writing plus a few hours of staring at the diff wondering if I was being too clever or not clever enough. The decisions themselves were small. The aggregate effect, after two weeks, is a system that's about as featureful as one I'd have built by importing five things, and significantly easier to live with for the next five years.

That's the trade. It almost always favors the small library. The case for the big one needs to clear a higher bar than most engineers, including me, instinctively make it clear.

Most days, when I check, it doesn't.
