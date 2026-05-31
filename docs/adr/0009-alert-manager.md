# ADR 0009 — Alert Manager + notifier contract

- **Status:** Accepted (Phase C-1)
- **Date:** 2026-05-31

## Context

After Phase B, CEP rules fired events into a metric counter and were
never delivered to a human. SLO burn rates couldn't be evaluated at
all. The platform was operationally blind: a real production outage
would be visible only to whoever happened to be reading `/metrics`.

Phase C-1 closes that loop with:

1. An `alert-manager` service that persists alert state, deduplicates
   firings, supports operator silences, and dispatches notifications.
2. A `pkg/notifier` abstraction with Slack, PagerDuty, and Webhook
   implementations.
3. A `pkg/sloburn` engine implementing the multi-window multi-burn-rate
   algorithm from the Google SRE Workbook.
4. A `pkg/alertsink` HTTP shim that bridges in-process CEP events
   (emitted by `pkg/actor`) to the out-of-process `alert-manager`.

## Decision

### Service boundary

Alert-manager is a **separate deployable**, not a library compiled
into ingest-gateway. Rationale:

- Alert dispatch is bursty and can stall on slow downstream APIs
  (PagerDuty, Slack). Keeping it out of the ingest critical path is
  worth one extra HTTP hop in the alert path.
- Operator silences and dedup state belong in Postgres — already a
  hard requirement for the tenant control plane. Co-locating alert
  state there is cheap.
- The fan-in shape ("many gateways + ml-anomaly-detector + future
  SLO consumers → one alert-manager") matches a service, not a
  library.

### SLO burn-rate algorithm

`pkg/sloburn` is the Google SRE Workbook canonical model:

| Pair | Long window | Short window | Burn factor | Severity |
|------|-------------|--------------|-------------|----------|
| 1    | 1 hour      | 5 minutes    | 14.4        | page     |
| 2    | 6 hours     | 30 minutes   | 6.0         | page     |
| 3    | 24 hours    | 2 hours      | 3.0         | ticket   |
| 4    | 72 hours    | 6 hours      | 1.0         | ticket   |

An alert fires when BOTH windows in a pair exceed the burn factor.
The pair with the highest severity wins. The "long window catches
sustained burn; the short window catches the early phase of the same
burn" intuition keeps false positives manageable for the paging tier.

Per-bucket retention is `max(LongWindow)` (so 72h by default at 1
sample/min = 4320 buckets/SLO/series). Memory cost is bounded; SLO
storage in Postgres for cross-restart persistence is Phase C-2 work.

### Notifier contract

`pkg/notifier.Notifier` is a one-method interface. Three
implementations ship:

- **Slack**: minimal `{"text": "..."}` payload (Block Kit is Phase D).
- **PagerDuty**: Events API v2 with action=trigger|resolve and the
  alert fingerprint as `dedup_key` so PD auto-closes resolved
  incidents.
- **Webhook**: POSTs the full Notification JSON for everything we
  don't ship a dedicated impl for.

`Dispatcher` fans out to many notifiers in parallel with bounded
concurrency and a per-notifier timeout (default 5s). A hung
downstream cannot stall the rest. Errors are joined and returned;
the alert-manager logs them but does not retry across the dispatcher
boundary — that lives in the alert-manager itself if/when we need it.

### Fingerprinting

`store.Fingerprint(tenant, ruleID, labels)` is `SHA-256(tenant ‖ NUL ‖
ruleID ‖ NUL ‖ sorted("k=v;"...))`. Stable across processes and
restarts, so dedup survives rolling deploys. ALL alert state keys are
fingerprint-based — no surrogate keys, no race conditions on first
sighting.

### Wire format: CEP → alert-manager

`pkg/alertsink.eventToWire` maps:

- `cep.HighErrorRate` → `severity=critical, rule_id=cep:HIGH_ERROR_RATE`
- `cep.HighLatency`   → `severity=warning,  rule_id=cep:HIGH_LATENCY`
- `cep.Event.Data["service"]` becomes a label, so different services
  produce different fingerprints and silences scope per-service.

The HTTP shim is **fire-and-forget**: a bounded buffer + worker
goroutine + 3-attempt retry with exponential backoff. Buffer
overflow increments a counter rather than blocking the actor — the
same CEP event will fire again on the next threshold-edge crossing
anyway.

### Postgres schema

Three tables: `alerts` (current state), `alert_history` (append-only
audit), `alert_silences` (operator suppressions). The same embedded-
migrator pattern as `services/tenant-api/store` — go-embed SQL files,
applied in lexical order, recorded in `schema_migrations`.

## Trade-offs

**Edge-triggered CEP + persistent alerts.** The CEP engine fires only
on threshold crossings (one event per breach, suppressed until the
metric dips below threshold). The alert-manager's "alert lasts until
resolved" semantics ride on top: a single CEP fire turns into a
firing alert that stays in `firing` state until explicitly resolved
or aged out. We get the best of both: no alert storm, persistent
state for the UI/timeline.

**No retry across the Dispatcher boundary.** A failed Slack post does
not get retried by the dispatcher itself. Justification: most failures
in this position are persistent (bad webhook URL, revoked token);
retrying just delays the operator's "Slack is broken" alert. If we
need durable dispatch, the right place to add it is *inside* a
Notifier wrapper that writes to a dead-letter queue.

**SLO state in process memory, not Postgres.** Phase C-1 keeps the
sliding-window counters in `sloburn.Evaluator` in process memory. A
restart loses up to `max(LongWindow)` worth of history per SLO. We
judge this acceptable for first ship because:

- The longest window is 72 hours; even 1-minute samples × 72 hours
  × 1000 SLOs = 4.3M counters, easily in-RAM.
- Loss-on-restart means at-worst a window's worth of false negatives
  immediately after a deploy; alerts re-fire within minutes.

Persistent SLO state lands in Phase C-2 if/when multi-instance
alert-manager becomes a requirement.

**HTTP push from CEP, not message queue.** A NATS topic would be
nicer architecturally (multiple consumers, replay), but stands up
NATS as a hard dependency for first-ship. The HTTP shim is one less
moving part for the first deploy; NATS lands in Phase C-3 alongside
the WASM-plugin event bus.

## Package changes

| Package                                    | Change |
|--------------------------------------------|--------|
| `pkg/sloburn/`                             | NEW. Multi-window multi-burn-rate evaluator. |
| `pkg/notifier/`                            | NEW. Notifier + Dispatcher + Slack/PagerDuty/Webhook. |
| `pkg/alertsink/`                           | NEW. EventSink impls (HTTPSink for prod, InMemorySink for tests). |
| `pkg/actor/`                               | Additive. `EventSink` in Options; default `NoopEventSink`. |
| `pkg/supervisor/`                          | Additive. `ActorOptions` propagated to every (re)created actor. |
| `pkg/engine/`                              | Additive. `SetAlertSink()` plumbs the sink through the supervisor. |
| `services/alert-manager/`                  | NEW service. HTTP /v1/events, /v1/observations, /v1/alerts, /v1/silences, /v1/slos. |
| `services/alert-manager/store/`            | NEW. Postgres-backed state + migrator. |
| `services/ingest-gateway/cmd/main.go`      | Additive. Wires HTTPSink when OBSERVE_X_ALERT_MANAGER_URL is set. |
