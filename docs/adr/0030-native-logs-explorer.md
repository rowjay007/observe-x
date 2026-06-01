# ADR-0030 — Native Logs explorer (Phase E-2)

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-2
* **Related**: ADR-0020 (alert SSE stream), ADR-0028 (visualization strategy), ADR-0029 (metrics workbench)

## Context

E-1 shipped the chart primitive and Metrics tab. Logs are the next
signal type with a clear native-UX win: an SRE looking at a spike
needs to drop straight into the log lines for that window without
context-switching to Grafana. The Logs tab needs two modes:

1. **Search** — point-in-time query with severity / service / body
   filters, returning the most recent N matches.
2. **Live tail** — open a stream and watch new lines arrive in real
   time, capped to a rolling window so the browser doesn't OOM.

## Decision

* **Search** uses the existing `POST /v1/query` ObserveQL endpoint.
  No new server-side surface; the SPA builds the SQL from the tab
  controls and parses the NDJSON response.

* **Live tail** ships a new SSE endpoint on `query-engine`:
  `GET /v1/logs/stream?tenant_id=X[&service=Y][&severity=Z]`.

  Implementation polls ClickHouse every 1s with a tenant-pinned
  cursor:

  ```sql
  SELECT timestamp, severity, service_name, body, trace_id,
         span_id, attributes
  FROM   logs
  WHERE  tenant_id = ?   -- bound, auth-derived
    AND  timestamp > ?   -- cursor, advanced each tick
    [AND service_name = ?]
    [AND severity     = ?]
  ORDER BY timestamp ASC
  LIMIT 200
  ```

  The cursor starts at `now()-5s` so a connecting client sees a few
  seconds of context, then advances strictly past the last emitted
  `timestamp` per tick. SSE framing matches the alert stream wire
  format from ADR-0020 — heartbeat every 15s, `event: log\ndata:
  {…json…}\n\n` for rows, `event: error` for terminal failures.

* **Frontend** virtualizes rendering via `requestAnimationFrame`
  chunks of 200 rows so a 500-row search doesn't block the UI
  thread; live-tail rows insert at the top with a sliding cap of
  500 lines in the DOM.

## Why polling instead of push from the ingest path

| Push (e.g. NATS subject per tenant) | Poll |
|---|---|
| Sub-second tail latency | 1s tail latency |
| New operational dependency (NATS hot for *every* log line) | Reuses the ClickHouse instance the operator already pays for |
| Forces ingest-gateway ↔ query-engine coupling | Services stay independent |
| Need separate auth on the bus | Reuses pkg/auth scopes |

The 1s tail latency is acceptable for the SRE workflow (the human
loop is already seconds-scale). If/when latency becomes a real
demand we can wire the existing NATS spillover (ADR-0024) topic as
a fan-out source under the same SSE wire shape — the SPA contract
won't change.

## Wire safety

* The handler binds `tenant_id`, `service`, `severity`, and the
  cursor via `?` parameters; the values never get string-formatted
  into the SQL, so injection is impossible by construction.
* `auth.GinRequireScope(auth.ScopeQuery)` gates the route — a key
  with `ingest` only gets a 403 with `WWW-Authenticate: scope=query`.
* `Cache-Control: no-cache, no-transform`, `X-Accel-Buffering: no`,
  and an explicit `flusher.Flush()` after every frame avoid the
  "nginx buffers SSE for 60 s" foot-gun.
* Per-tick `LIMIT 200` caps how many rows a single tick can emit;
  a slow consumer can't make the handler buffer unboundedly because
  the next tick simply advances the cursor past anything older.

## Trade-offs

| ✓ | ✗ |
|---|---|
| Zero new infra dependencies. | 1s lower-bound tail latency. |
| Reuses ClickHouse `(tenant_id, service_name, timestamp)` ORDER BY for fast cursor advance. | Wakes the database every 1s per active tail subscriber. |
| Same SSE wire shape as alert stream — SPA reuses the parseSSE helper. | High-volume tenants might want websockets for ack-aware backpressure (deferred). |
| Caller-supplied filters are parameterised (no injection risk). | Filter set is limited to service + severity; richer LogQL-style filters land in E-5. |

## What ships

| File | Purpose |
|---|---|
| `services/query-engine/cmd/logs_sse.go` | SSE handler + cursor-poll loop + heartbeat. |
| `services/query-engine/cmd/logs_sse_test.go` | `timestampOf` unit tests covering driver / RFC3339 / nil paths. |
| `services/query-engine/internal/executor/executor.go` | Adds `Client()` accessor so the SSE handler can run hand-written SQL on the same connection pool. |
| `services/query-engine/cmd/main.go` | New route: `GET /v1/logs/stream` behind `ScopeQuery`. |
| `services/ui-server/cmd/assets/app.js` | Logs panel module (search + tail toggle) shipped in E-1. |

## Acceptance criteria

* `go build ./services/query-engine/cmd` succeeds.
* `go test ./services/query-engine/cmd` covers the timestamp parser.
* Manual: `curl -N -H "X-Tenant-ID: acme" -H "Authorization: Bearer …" \
  http://localhost:7100/v1/logs/stream` streams heartbeats every
  15s and `log` events whenever ClickHouse `logs` table receives
  a new row for `acme`.
* `ui-server` Logs tab: search returns results within 2s; live-tail
  status pill flips to "live" on click and back to "offline" on
  stop, with no leaked goroutines (`AbortController` covers cancel).
