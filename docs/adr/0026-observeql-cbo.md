# ADR-0026 — Cost-Based Optimiser for ObserveQL

- Status: Accepted
- Date: 2026-06-01
- Phase: D-9

## Context

The Phase B planner is rule-based: it tenant-pins, maps the
source to a table, clamps `LIMIT`, and emits one ClickHouse
SELECT. That's correct but loses every "two equivalent queries
at different cost" opportunity — chiefly join ordering and CTE
materialisation. As ObserveQL grows joins (planned for Phase D+),
the rule-based planner will start picking demonstrably bad plans.

We need a seam to introduce cost-based optimisation without
disrupting the existing planner.

## Decision

Add `pkg/observeql/cbo.go`:

- `Stats` interface — pluggable cardinality + NDV provider. The
  default impl will read ClickHouse `system.parts` /
  `system.columns`; tests inject `StubStats`.
- `CostModel` — assigns numeric cost to a `PhysicalPlan`. Cost =
  rows-scanned + `ShuffleWeight` × rows-shuffled (mirrors
  Trino's iterative optimiser heuristic).
- `EnumeratePlans(ctx, ast, opts, stats)` returns candidate
  physical plans. Plan 0 is always the rule-based baseline so
  the CBO can never regress.
- `ChooseBest(model, plans)` picks the cheapest with deterministic
  tie-break.

Phase D-9 ships the seam + the rule-based plan. JOIN reordering
and predicate pushdown enumeration land in follow-up PRs once
ObserveQL grows the requisite AST shapes.

## Trade-offs

- **Skeleton-first, full optimiser later** — shipping the seam
  unlocks incremental rule additions without re-plumbing call
  sites. The cost is that today the CBO produces exactly one
  plan and adds no benefit; this is acceptable as a foundation.
- **Stats are best-effort** — when `Stats.TableRows` fails we
  fall back to 0 rows scanned; the rule-based plan still wins
  by Variant-name tie-break. The system stays safe under stats
  outage.
- **Cost model is heuristic, not learned** — proven adequate
  for shipping cost-aware databases (Postgres, MySQL, Trino).
  ML-based CBO (Microsoft's MSCN, Meta's Bao) is a research
  frontier and out of scope.

## Package changes

- `pkg/observeql/cbo.go` — new file.
- `pkg/observeql/cbo_test.go` — enumerate, choose best,
  cost-model assertions, stub-stats fallback.

## Verification

- `go test -race ./pkg/observeql/...`
- Manual demo: enumerate with two competing variants (different
  `RowsScanned`), confirm the cheaper one wins.
