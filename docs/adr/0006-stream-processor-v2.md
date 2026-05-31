# ADR 0006 — Stream-processor v2

- **Status:** Accepted (Phase B-4)
- **Date:** 2026-05-29
- **Supersedes parts of:** ADR-0001 (per-tenant actor sketch)

## Context

The Phase A stream-processor primitives were skeletons:

1. `pkg/supervisor.checkHealth` deleted crashed actors instead of
   restarting them. A single tenant crash silently leaked traffic.
2. `pkg/cep.HighErrorRateRule` divided a running counter by the
   magic number `100.0` and called the result a rate. The threshold
   was meaningless and the rule fired on every signal once the
   counter passed the gate, producing alert storms.
3. `pkg/sampling.AdaptiveSampler` scored every trace against
   hard-coded constants (`severity=ERROR ⇒ +100`, `duration>1s ⇒ +50`).
   Latency was binary: "over a second or not." Anomalies *relative
   to baseline* — the actual signal for adaptive sampling — were
   invisible.

## Decision

### Supervisor: OTP-flavoured one-for-one restart with quarantine

```
crash → backoff(250ms→30s, ±20% jitter, capped) → restart
        if restarts > 5 in 60s → quarantine
                                   ↓
                                operator runs ReleaseQuarantine
```

The supervisor monitors actors on a 5s tick (configurable). A crashed
actor's `IsRunning()` returns false; the supervisor records the
restart timestamp in a per-actor sliding window. If the window holds
more than `MaxRestarts` timestamps the actor is marked **quarantined**,
all subsequent routes for that tenant are dropped (counted in
`TotalDropped` for alerting), and the supervisor will not auto-restart
until an operator calls `ReleaseQuarantine(tenantID)`.

Borrowed terminology — not a faithful OTP implementation. Tenant
actors share no state, so `one_for_one` is the only meaningful
strategy; `rest_for_one` and `one_for_all` aren't relevant.

### CEP: per-service sliding window, edge-triggered

Each rule keeps a ring of 1-minute buckets covering a configurable
window (default 5 minutes). On every signal the active bucket is
incremented; the rate is `sum(buckets) / window_seconds`. The rule
fires **once per crossing** — the `firing[service]` flag is set on
the first breach and cleared the moment the rate dips below
threshold. This prevents the Phase A "alert per signal after the
first" storm.

Two production rules ship:

| Rule                | Trigger                                                  |
|---------------------|----------------------------------------------------------|
| `HighErrorRateRule` | per-service ERROR rate (errors/sec) above threshold      |
| `HighLatencyRule`   | per-service peak latency (ms) in window above threshold  |

`HighLatencyRule` uses peak (max) latency in the window — cheap and
sufficient for alerting. Real p95/p99 percentile estimators (t-digest,
HDR) are Phase C work; they're worth the cost only once the alert
manager actually consumes percentile thresholds.

### Sampler: EWMA z-score baseline + optional persistence

Score formula:

```
score = base                       (severity, gross-latency, special services)
      + 10 × max(0, latency_zscore_against_service_baseline)
      + 25 if attributes["parent_sampled"] == "true"
```

`pkg/sampling/ewma.go` implements a per-key EWMA tracker with
`alpha=0.05` (~20-sample half-life). The z-score is gated until 30
observations have accumulated for that key — a single cold-start
outlier doesn't get to dominate the score before there's a baseline
to compare against.

State persistence is via a `StateStore` interface:

| Implementation     | When to use                          |
|--------------------|--------------------------------------|
| `InMemoryStore`    | Single-instance dev / tests          |
| `RedisStore`       | Multi-instance prod                  |

A background goroutine flushes `latency.snapshot()` to the store
every 30 seconds; on construction the sampler hydrates from the
store. The hot path never blocks on Redis — flush failures are
logged-and-continue. Redis is *optional*: if no store is wired, the
sampler degrades gracefully to per-process learning.

## Trade-offs

**Quarantine vs. infinite restart.** An actor that crashes every
250ms wastes CPU and produces useless logs. Quarantine fails loudly:
the `Quarantined` stat is a Prometheus gauge; alerts on it tell the
operator a bad tenant is poisoning the system. The cost is one
tenant going dark until human intervention; we judge this cheaper
than a silent CPU-burning restart loop.

**Edge-triggered rules vs. continuous firing.** Edge means you get
one event per condition transition. If the alert manager wants
"reminder" events every N minutes while the condition persists, that
becomes the alert manager's job (Phase C). Cleaner separation.

**EWMA max-as-p95.** Peak latency in a window overestimates p95 in
the presence of one bad sample. That's the right side of the
trade-off for alerting (false-positive > false-negative) but wrong
for capacity planning. Phase C adds proper percentile estimators
when the query layer needs them.

**Redis optional, not required.** Multi-instance stream processors
will eventually need shared sampler state to avoid every replica
relearning baselines from scratch after a deploy. Until we have
multi-instance, the operational cost of running Redis isn't justified.
The interface is in place; flipping it on is one env var.

## Package changes

| Package                  | What changed                                                                                          |
|--------------------------|-------------------------------------------------------------------------------------------------------|
| `pkg/supervisor`         | Whole file rewritten. New `Options`, `RouteToTenant` accounting, `ReleaseQuarantine`, jittered backoff. |
| `pkg/cep`                | Whole file rewritten. Sliding window primitive, two production rules, edge-triggered firing.          |
| `pkg/sampling/ewma.go`   | New. EWMA mean + variance + z-score with cold-start gating.                                           |
| `pkg/sampling/state.go`  | New. `StateStore` interface, `InMemoryStore` default.                                                 |
| `pkg/sampling/state_redis.go` | New. `RedisStore` using `github.com/redis/go-redis/v9`.                                         |
| `pkg/sampling/sampler.go`| New `SamplerOptions`, additive Score factors, hydration on construct, flush goroutine on Close.        |

## Backward compatibility

- All existing public Go APIs preserved (`NewAdaptiveSampler`,
  `NewSupervisor`, `NewHighErrorRateRule`, `Engine`).
- `NewHighErrorRateRule(tenant, window, 0.05)` — the existing actor
  call — used to mean "5% of 100 = 5 errors per evaluation"; it now
  means "5 errors/second over the window." Tighten the threshold in
  the actor at the same time as merging this ADR (see `pkg/actor`).
