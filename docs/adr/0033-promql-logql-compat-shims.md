# ADR-0033 — PromQL / LogQL compatibility shims (Phase E-5)

* **Status**: Accepted
* **Date**: 2026-06-01
* **Phase**: E-5
* **Related**: ADR-0022 (ObserveQL), ADR-0028 (visualization strategy), ADR-0029 (metrics workbench), ADR-0030 (logs explorer)

## Context

Operators migrating from Prometheus + Loki have **years** of
PromQL/LogQL expressions sitting in:

* Grafana dashboard JSONs.
* Alerting rules (Prometheus rule files / Loki ruler).
* Runbooks ("see `histogram_quantile(0.99, …)` in tile 4").

Forcing every one of those to be rewritten to ObserveQL is a
non-starter for an adoption story. ADR-0028 ships the Grafana
integration via `grafana-clickhouse-datasource`, but that asks
operators to rewrite the queries to ClickHouse SQL. We need a path
where the **panel keeps working** when the datasource type is
flipped from Prometheus → ObserveX.

## Decision

Ship two new packages and a small handler that lights up
Prometheus- and Loki-shaped HTTP endpoints on `query-engine`:

| Package | What it does |
|---|---|
| `pkg/promql` | Tokenize → AST → ClickHouse SQL for a documented PromQL subset. |
| `pkg/logql`  | Same shape, for a LogQL subset against the `logs` table. |

| Endpoint | Translates to |
|---|---|
| `GET\|POST /prom/api/v1/query`        | PromQL instant query → matrix result with one timestamp. |
| `GET\|POST /prom/api/v1/query_range`  | PromQL range query → matrix result, `start/end/step` honoured. |
| `GET\|POST /loki/api/v1/query`        | LogQL instant query. |
| `GET\|POST /loki/api/v1/query_range`  | LogQL range query — log query → streams, metric query → matrix. |

All four are gated by the same `auth.GinRequireScope(query)` that
fronts `/v1/query` — the tenant_id comes from auth, never from the
query string.

### Supported PromQL subset

```
Vector selector:   metric{label="v",label!="v",label=~"re",label!~"re"}
Range selector:    metric[5m]
Aggregations:      sum|avg|min|max|count [by|without (labels)] (vector)
Range functions:   rate(v[d])  irate(v[d])  increase(v[d])
                   {avg|sum|min|max|count}_over_time(v[d])
                   quantile_over_time(phi, v[d])
Quantile:          quantile(phi, vector)
Binary ops:        aggr OP scalar | scalar OP aggr  (+,-,*,/,>,<,>=,<=,==,!=)
Durations:         ms, s, m, h, d, w, y
```

### Supported LogQL subset

```
Stream selectors:  {service="x", severity=~"ERR.*", customLabel="v"}
                   service / service_name → service_name column
                   severity / level       → severity column
                   trace_id / span_id     → first-class columns
                   anything else          → attributes['<key>']
Line filters:      |= "substr"   != "substr"
                   |~ "regex"    !~ "regex"
Metric over log:   count_over_time({...}[5m])
                   rate({...}[5m])           = count / step seconds
                   bytes_over_time({...}[5m]) = sum(length(body))
```

### Explicitly NOT supported

* PromQL: `topk`/`bottomk`/`histogram_quantile`/subqueries/`@`/`offset`/vector-on-vector binops/group_left/group_right.
* LogQL: `unwrap`/`parser`/`label_format`/`stddev_over_time`/json filters.
* Recording rules / alerting rules (queue with the rules engine).

All four endpoints return a structured `{status:"error",
error:"…"}` body on `ErrUnsupported` so Grafana surfaces the
exact failure to the user.

### Safety model

1. **No injection.** Every value in matcher predicates, line
   filters, and time bounds is bound as a `?` parameter. The
   translator refuses to *interpolate* user-controlled strings
   into SQL — even label *names* go through a strict `\w/-.` filter
   before they become an `attributes['key']` lookup (anything else
   becomes `INVALID`, which intentionally produces a query that
   matches nothing).
2. **Tenant pinning.** After translation, `runProm` / `runLoki`
   wrap the inner SQL with `SELECT * FROM (…) WHERE tenant_id = ?`
   and append the auth-derived tenant to the args. The PromQL
   string can NEVER name a tenant.
3. **Scope enforcement.** All shim routes require `ScopeQuery`,
   identical to the native `/v1/query` route.
4. **Bounded execution.** Inherits the executor's timeout and
   max-rows defaults from the existing `/v1/query` path — the
   compatibility layer is *purely* a translator, not a separate
   execution pool.

### Sample translations

PromQL:
```
sum(rate(http_requests_total{service="api"}[5m])) by (code)
```
→
```sql
SELECT toStartOfInterval(timestamp, INTERVAL 30 SECOND) AS t,
       sum(value) AS v, attributes['code'] AS lbl_code
FROM   metrics
WHERE  metric_name = 'http_requests_total'
  AND  attributes['service'] = 'api'
  AND  timestamp BETWEEN ? AND ?
GROUP BY t, lbl_code
ORDER BY t
```
(the `rate()` step happens inside the bucket via `(max-min)/step`)

LogQL:
```
{service="checkout-api"} |~ "error|fail" != "skip"
```
→
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

## Trade-offs

| ✓ | ✗ |
|---|---|
| Pre-existing PromQL / LogQL panels keep working with zero changes. | We own a small parser-translator forever. |
| Zero new infra — translation happens in the query-engine. | The translated SQL is not identical to native Prometheus / Loki semantics for edge cases (e.g. `rate()` approximation across buckets). |
| Tenant-pinning + scope checks reuse the existing security plumbing. | Power users with `histogram_quantile`-heavy panels still need rewrite or stay on Grafana ClickHouse datasource (E-0). |
| Documented subset is small enough to reason about exhaustively. | Subset → full coverage is an open-ended ask; we'll grow it as concrete demand surfaces. |
| Injection safety verified by unit tests (`TestLabelMatcherSafety`, `TestInjectionSafety`). | Translator quality scales with test surface — we keep adding to the rejection / acceptance matrices as users hit edges. |

## What ships

| File | Purpose |
|---|---|
| `pkg/promql/promql.go` | Public `Translate(QueryParams) (*Result, error)`. |
| `pkg/promql/lex.go` | Hand-rolled tokenizer (no external deps). |
| `pkg/promql/parser.go` | Recursive-descent AST builder for the documented subset. |
| `pkg/promql/translate.go` | AST → ClickHouse SQL lowering. |
| `pkg/promql/promql_test.go` | 25+ tests: aggregations, range fns, binary ops, rejections, injection safety, duration parsing. |
| `pkg/logql/logql.go` | Full LogQL subset translator + tests. |
| `pkg/logql/logql_test.go` | 30+ tests: matchers, line filters, metric over log, rejections, label mapping, injection safety. |
| `services/query-engine/cmd/shims.go` | Eight Gin handlers + tenant-wrapping + response shaping. |
| `services/query-engine/cmd/main.go` | `registerShims(authorized, …)` wired into the existing router. |

## Acceptance criteria

* `go test ./pkg/promql ./pkg/logql` — all unit tests pass.
* `go build ./services/query-engine/cmd` — clean.
* `curl 'http://localhost:7100/prom/api/v1/query_range?query=sum(rate(rps[5m]))&start=…&end=…&step=30s' \
   -H 'X-Tenant-ID: acme' -H 'Authorization: Bearer …'`
  returns a Prometheus-shaped `{status:"success", data:{resultType:"matrix",result:[…]}}`.
* `curl '…/loki/api/v1/query_range?query={service="x"}|="error"&start=…&end=…' …`
  returns a Loki-shaped `{status:"success",data:{resultType:"streams",result:[…]}}`.
* Unsupported expression (e.g. `topk(5, rps)`) → 400 with
  `{status:"error", errorType:"bad_data", error:"promql: parse: …"}`.
* Injection attempts in label values bind as `?` parameters in SQL
  (verified by `TestLabelMatcherSafety`).
