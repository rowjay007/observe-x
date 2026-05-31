# ADR 0007 — ObserveQL and the query-engine

- **Status:** Accepted (Phase B-3)
- **Date:** 2026-05-29

## Context

Until Phase B-3, ObserveX was write-only. The roadmap calls for a
distributed query engine with a custom DSL (ObserveQL), an ANTLR4-
generated parser, a cost-based optimiser, and Arrow IPC result
streaming. That is a multi-month effort.

Phase B-3 lands a **functional read path** today and pays down the
grammar/optimiser/Arrow debt incrementally. The choices below pick
the smallest possible Phase-B slice that is genuinely useful and
genuinely safe, not one that ships an artefact and calls it done.

## Decision

### Grammar via participle, not ANTLR4

`pkg/observeql` uses `github.com/alecthomas/participle/v2` to define
the grammar directly in Go struct tags. Why not ANTLR4:

1. **No code generation in the build.** ANTLR4 requires a separate
   tool, a separate runtime, and a generated tree of files that
   you-don't-touch-but-must-commit. Participle is pure Go;
   `go build` is the entire toolchain.
2. **The grammar is small.** The current spec is ~60 lines of Go
   struct tags. ANTLR4's win is at the scale of full SQL or
   JavaScript; for a domain DSL where we control the surface, the
   ROI flips.
3. **Production engineers can read it.** Participle grammars live
   next to the AST they parse; the AST IS the grammar plus return
   types. ANTLR4 forces context-switching across `.g4` files and
   generated visitors.

If/when ObserveQL grows past what participle handles cleanly
(left-recursive precedence climbing, error-recovery with span
highlighting), we can re-evaluate. The migration cost is "rewrite
the grammar"; the AST consumers downstream don't move.

### Tenant-safety is in the planner, not the executor

`PlanQuery` always emits `tenant_id = ?` as the **first** predicate
with the auth-derived tenant id as the **first** parameter. A
caller's WHERE clause is appended with AND, so even a deliberately
crafted `WHERE tenant_id = 'other'` becomes:

```
WHERE tenant_id = ? AND (... AND tenant_id = 'other')
       ^^^                                ^^^^^^^
       trusted tenant id                  caller-supplied; useless
```

The trusted predicate ALWAYS holds; cross-tenant access is
mechanically impossible regardless of clause cleverness. Verified by
`TestPlanRejectsCallerSuppliedTenant`.

Identifiers (columns, group-by, function args) are validated against
a hard-coded per-source **allow-list** (`allowedColumnsPerSource`).
Anything that isn't in the allow-list is rejected at plan time, so
the executor never sees an unvalidated identifier. No DDL, no DML —
the parser doesn't even recognise the keywords.

Function calls follow the same pattern: `allowedFunctions` is a tiny
set (`count`, `avg`, `sum`, `min`, `max`). Adding a new aggregate is
one line.

### NDJSON now, Arrow IPC in Phase B-3.5

NDJSON is `application/x-ndjson` — a header object, one row per line,
a trailer object with timing. Trade-offs vs Arrow:

| Aspect            | NDJSON                            | Arrow IPC                           |
|-------------------|-----------------------------------|-------------------------------------|
| Client tooling    | `curl \| jq` works                | needs Arrow library                 |
| Wire size         | ~2-3× Arrow on numeric workloads  | smallest                            |
| Encode/decode CPU | json.Encoder, simple              | columnar buffers, ~5x faster        |
| Streaming         | line-delimited, trivial           | record-batch framed, less trivial   |
| Phase-B effort    | half a day                        | several days                        |

NDJSON is the right answer for a beta read path. Arrow lands in
B-3.5 behind the same `Execute(ctx, plan, w)` signature — codec
selection becomes a request flag.

### Time column is per-source

`timeColumnPerSource` maps `metrics` and `logs` to `timestamp`,
`traces` to `start_time` (the schema's actual time column). The
implicit SINCE filter and ORDER BY both consult this map. A future
table that uses a different time column adds one line.

### Auth is shared with ingest-gateway

The query-engine wires the same `pkg/auth.PostgresKeyStore` against
the same Postgres tenant control plane. A tenant's existing API key
authorises both write (ingest) and read (query). Read-only / write-
only scopes land in Phase C alongside operator OIDC.

### Limit cap

`MaxRowLimit` defaults to 10 000 and caps any caller-requested
LIMIT. The cap is enforceable from the request body
(`max_rows`) but never increased above the global ceiling. Bigger
result sets need pagination (cursor support is Phase C).

### Query timeout

`timeout_secs` in the request body, capped at 300s; default 30s.
Bound on the `context.Context` so a long-running scan can't leak a
connection.

## Consequences

### Positive

- We can answer "show me errors in the last hour" today, against
  the same data the ingest-gateway is writing.
- The grammar is small enough to understand in one read; reviewer
  cognitive load is bounded.
- Tenant safety is one-test-and-done because the injection sits at
  a single chokepoint in the planner.
- The codec is swappable: NDJSON now, Arrow in B-3.5, no API churn.

### Negative

- No JOINs. Cross-source correlation (e.g. logs that match the
  trace_id of a slow span) is Phase C work — needs a query graph
  abstraction the planner doesn't have yet.
- No subqueries / CTEs. The grammar deliberately stops at the
  smallest useful surface; adding them later is additive.
- Allow-list of columns means schema changes require a code change.
  This is the right default for safety; an opt-in "raw mode" for
  trusted internal callers may land in Phase C.

### Deferred to Phase B-3.5

- Arrow IPC encoder.
- Cost-based optimiser (today's planner is rule-based: one source,
  one time predicate, no joins, so there's nothing for the optimiser
  to choose between).
- Federated execution against S3 + Parquet cold tier.

## Package changes

| Package                                  | Change |
|------------------------------------------|--------|
| `pkg/observeql/`                         | NEW. AST, parser, planner. |
| `services/query-engine/`                 | NEW service. HTTP /v1/query, NDJSON streaming, shared auth. |
| `github.com/alecthomas/participle/v2`    | NEW dep. Parser combinator. |

No existing packages were modified by this ADR.
