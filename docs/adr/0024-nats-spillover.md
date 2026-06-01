# ADR-0024 — NATS JetStream spillover for the supervisor

- Status: Accepted
- Date: 2026-06-01
- Phase: D-7

## Context

Phase B-4 sized the per-tenant supervisor mailbox at 4096 entries
and dropped on overflow. The drop counter is observable, the
behaviour is correct for "tenant is flooding, apply back-pressure,"
but it's hostile to transient downstream stalls — a 30-second
ClickHouse merge storm or a deploy gap turns into observable data
loss on the dashboard.

We want the back-pressure semantics for sustained overload AND a
durable side-car for transient ones.

## Decision

Add `pkg/spillover` — a thin wrapper around NATS JetStream:

- `Spillover.Push(ctx, tenantID, signal)` durably enqueues a
  signal on the `observex.spillover.<tenant>` subject when called.
- `Spillover.Consume(ctx, route)` runs a durable pull consumer
  that drains the stream and routes signals back through the
  supervisor at the rate it can accept.
- `New` returns `(nil, nil)` when `OBSERVE_X_NATS_URL` is empty,
  so spillover gracefully degrades to legacy drop-on-full.

Supervisor integration via `Spiller` interface (in
`pkg/supervisor`) — keeps `pkg/supervisor` free of a NATS
dependency for tests. `RouteToTenant` now tries `Spillover.Push`
before dropping; the drop counter only increments when both paths
fail.

## Trade-offs

- **NATS JS over Kafka / Redis Streams** — NATS JS is the
  smallest durable queue we can run; a single statefulset replica
  serves the entire spillover need at our scale. Kafka would
  pull in ZK / Kraft + a JVM dep; Redis Streams isn't durable to
  fsync without enterprise.
- **At-most-once semantics on consume errors** — poison messages
  (failed JSON unmarshal) get `msg.Term()` rather than retry. The
  alternative — infinite retry — would cascade into delivery
  storms when the stream itself is bad.
- **Spillover is best-effort** — when NATS is down the supervisor
  reverts to legacy drop. That's a degraded mode, not a failure
  mode; the system stays up.
- **Push is synchronous** — adds ~250 µs to the slow path. We
  bound it with a 250 ms timeout so a NATS hang doesn't
  back-propagate into ingest.

## Package changes

- `pkg/spillover` — new package + tests.
- `pkg/supervisor/supervisor.go` — `Spiller` interface,
  `Options.Spillover`, branch in `RouteToTenant`,
  `Stats.TotalSpilled`.
- `pkg/engine/engine.go` — `Options.Spillover` plumbed through.
- `github.com/nats-io/nats.go` added to `go.mod`.

## Configuration

- `OBSERVE_X_NATS_URL=nats://nats:4222` — enable spillover.

## Verification

- `go test -race ./pkg/supervisor/... ./pkg/spillover/...`
- Integration test (not in CI; run locally): boot `nats-server -js`,
  flood a single tenant past mailbox capacity, observe
  `observex_supervisor_spilled` increase while
  `observex_supervisor_dropped` stays flat.
