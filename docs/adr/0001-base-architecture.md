# ADR 0001 — Base architecture

- **Status:** Accepted (Phase A)
- **Date:** 2026-05-29
- **Owner:** Platform team

## Context

ObserveX is a multi-tenant, self-hosted observability platform. The
roadmap defines five phases ending in a GA-ready product replacing
commercial APM vendors. Phase A is a hardening pass over the Phase 1
foundation: ingest gateway, WAL, ClickHouse strategy, per-tenant
actors. Before any further phase compounds onto the existing code, we
need a documented architectural seam that reviewers and future
contributors can rely on.

## Decision

### Layered shape

```
cmd/                       binary entry points (one per service)
services/<svc>/cmd         service composition root (main.go)
services/<svc>/internal/   service-private code (receivers, handlers)
pkg/<feature>/             cross-service library packages, the
                           canonical source of truth for shared types
infrastructure/            Helm/Terraform (Phase C)
proto/                     protobuf source (generated stubs land in pkg)
docs/adr/                  architecture decision records
```

`pkg/` is the single source of truth. Any duplication between
`services/<svc>/internal/...` and `pkg/...` (as found in pre-Phase-A
state for WAL, actor, supervisor, and pipeline) is a bug.

### Ingest data flow

```
Client → gateway receiver → ProcessSignal(non-blocking)
        ↓ ingestCh (bounded, 65536)
        Decode → Validate → Enrich (bounded, 1024 each)
        ↓
        Worker pool (GOMAXPROCS)
        ↓
        Single sampling decision → WAL (durable) → Backend (async)
                                                  → Actor mailbox
```

Three properties are non-negotiable:

1. **Back-pressure originates at the bounded ingest channel.** Senders
   get an immediate ErrOverloaded (HTTP 429 / gRPC ResourceExhausted)
   when the pipeline is saturated. Blocking writes are forbidden.
2. **The WAL is the durability root.** ClickHouse is a serving layer
   whose breaker can open without losing data. Phase B will add a
   WAL-to-ClickHouse replay tool exactly because of this property.
3. **Sampling has exactly one owner — the engine.** Per-tenant actors
   may inspect signals (CEP, metrics) but MUST NOT make persistence
   decisions. Two owners produces non-deterministic, untestable
   behavior (the pre-Phase-A code had this bug).

### Component responsibilities

| Component | Owns | Does NOT own |
|---|---|---|
| `pkg/engine` | pipeline composition, sampling decision, WAL/storage hops | OTLP decoding, transport |
| `pkg/wal` | durable append, recovery, group commit | semantic understanding of payload |
| `pkg/storage/clickhouse` | batching, circuit breaker, schema migrations | sampling, durability |
| `pkg/actor` + `pkg/supervisor` | per-tenant CEP, lifecycle | sampling, persistence |
| `pkg/sampling` | scoring + Keep/Drop decision | enforcement |
| `pkg/observability` | Prometheus metrics, pprof handlers | service-specific logic |
| `services/ingest-gateway` | transport (HTTP/gRPC/StatsD), auth, OTLP adaptation | pipeline mechanics |

### Why Go 1.24

`github.com/ClickHouse/clickhouse-go/v2` v2.46+ requires Go 1.24.1.
The roadmap specified 1.23+; 1.24 is forward-compatible and adds the
language features (range-over-func, performance improvements) we will
lean on in Phase B's query engine.

## Consequences

### Positive

- One canonical location per concept. Code review can mechanically
  reject re-introduction of duplicate packages.
- The engine has a single back-pressure path, which makes capacity
  planning a tractable single-variable problem (ingest channel size).
- ClickHouse failures degrade gracefully instead of cascading. The WAL
  + breaker + async-flush triple is what lets the platform survive a
  ClickHouse outage without dropping signals.

### Negative

- Anything Phase B builds must respect the `pkg/` ownership rules,
  which can feel restrictive when prototyping. The reviewer's job is
  to push prototypes into `pkg/` once a stable shape emerges.
- Go 1.24.1 bump may force minor upgrades in unrelated tooling.

## Alternatives considered

- **Hexagonal / clean architecture layout with full ports & adapters.**
  Over-engineered for the current team size. Revisit in Phase B if the
  query engine grows enough to justify the cost.
- **Allow `services/<svc>/internal` to fork shared types.** Rejected.
  Duplicate WAL implementations is exactly how we got the bug where
  one timestamp encoding was `binary.LittleEndian.Uint64([]byte(
  fmt.Sprintf("%d", 0)))` — i.e. always 0.
