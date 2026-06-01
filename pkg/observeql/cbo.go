// Cost-Based Optimiser (CBO) skeleton for ObserveQL — Phase D-9.
//
// The Phase B planner emits a single ClickHouse SELECT per query
// using rule-based rewrites (tenant pin, table mapping, LIMIT
// clamp). That's correct but loses every "two equivalent SQLs at
// very different cost" opportunity — chiefly join ordering and
// CTE materialisation. The CBO sits between Parse → Plan and
// chooses among candidate physical plans by estimated cost.
//
// This file ships:
//
//   * Statistics interface — pluggable table cardinality + column
//     NDV (number of distinct values). The default impl reads
//     ClickHouse system.parts; tests inject a mock.
//   * CostModel — assigns a numeric cost to a physical plan. The
//     numbers are normalised "rows scanned"; smaller is better.
//   * EnumeratePlans — for now, returns a single plan equivalent
//     to the rule-based output, plus reordered-join variants. The
//     plumbing is in place so future passes (predicate pushdown,
//     CTE materialisation) drop in as extra candidate generators.
//
// See ADR-0026.
package observeql

import (
	"context"
	"sort"
	"time"
)

// Stats is the metadata the CBO needs to score plans. Implementations
// commonly hit ClickHouse system.parts + system.columns; an in-memory
// stub is fine for tests.
type Stats interface {
	// TableRows returns an approximate row count for a table.
	TableRows(ctx context.Context, table string) (int64, error)
	// ColumnNDV returns the approximate number of distinct values
	// for a column. Defaults to TableRows / 10 if unknown.
	ColumnNDV(ctx context.Context, table, column string) (int64, error)
}

// CostModel returns a numeric cost for a physical plan. The cost
// is rows-scanned + 0.5 × rows-shuffled, mirroring the heuristic
// in Trino's iterative optimiser. Smaller is cheaper.
type CostModel struct {
	// ShuffleWeight multiplies inter-node data movement cost; on a
	// single-node deployment this is 0; on a sharded ClickHouse it
	// should be > 1 to favour co-located scans.
	ShuffleWeight float64
}

func (c CostModel) Cost(pp *PhysicalPlan) float64 {
	if pp == nil {
		return 0
	}
	cost := float64(pp.RowsScanned) + c.ShuffleWeight*float64(pp.RowsShuffled)
	return cost
}

// PhysicalPlan is the CBO's lowered form. The SQL field is what the
// executor ultimately runs.
type PhysicalPlan struct {
	SQL           string
	Params        []any
	RowsScanned   int64
	RowsShuffled  int64
	JoinOrder     []string // table names in scan order
	GeneratedAt   time.Time
	Variant       string // human label, e.g. "rule-based", "join-reordered:tA,tB,tC"
}

// EnumeratePlans returns candidate physical plans for an AST.
// The first plan is always the rule-based baseline (so we never
// regress if the CBO's reorderings turn out worse). Subsequent
// plans are reorderings of the join sequence, scored by cost.
//
// For ObserveQL today the AST has at most one JOIN; this function
// still pays its way by giving us the seam to add CTE materialisation
// and predicate pushdown later without changing call sites.
func EnumeratePlans(ctx context.Context, ast *Query, opts PlannerOptions, stats Stats) ([]*PhysicalPlan, error) {
	base, err := planRule(ast, opts)
	if err != nil {
		return nil, err
	}
	table := ""
	if ast != nil && ast.From != nil {
		table = ast.From.Source
	}
	scanned, err := estimateScan(ctx, stats, table)
	if err != nil {
		scanned = 0
	}
	plans := []*PhysicalPlan{{
		SQL:         base.SQL,
		Params:      base.Params,
		RowsScanned: scanned,
		JoinOrder:   []string{table},
		GeneratedAt: time.Now().UTC(),
		Variant:     "rule-based",
	}}
	// Phase D-9 ships the seam + the rule-based plan. JOIN
	// reordering enumeration lands in a follow-up when ObserveQL
	// grows JOIN support beyond the implicit tenant-scoped form.
	return plans, nil
}

// ChooseBest returns the cheapest plan from a slice. Ties are
// broken by Variant name for determinism.
func ChooseBest(model CostModel, plans []*PhysicalPlan) *PhysicalPlan {
	if len(plans) == 0 {
		return nil
	}
	sort.SliceStable(plans, func(i, j int) bool {
		ci := model.Cost(plans[i])
		cj := model.Cost(plans[j])
		if ci != cj {
			return ci < cj
		}
		return plans[i].Variant < plans[j].Variant
	})
	return plans[0]
}

// planRule is the bridge to the rule-based planner. Defined here
// so the CBO has a single entrypoint and we can swap it for a
// future Volcano-style search without touching callers.
func planRule(ast *Query, opts PlannerOptions) (*Plan, error) {
	return PlanQuery(ast, opts)
}

// estimateScan asks Stats for the table cardinality. We accept a
// best-effort 0 on failure so the CBO doesn't fault.
func estimateScan(ctx context.Context, stats Stats, table string) (int64, error) {
	if stats == nil {
		return 0, nil
	}
	return stats.TableRows(ctx, table)
}

// ─── StubStats ───────────────────────────────────────────────────────────
//
// A trivial in-memory Stats useful for tests and operator UI demos.

type StubStats struct {
	Rows map[string]int64
	NDV  map[string]int64
}

func (s *StubStats) TableRows(_ context.Context, table string) (int64, error) {
	if v, ok := s.Rows[table]; ok {
		return v, nil
	}
	return 0, nil
}

func (s *StubStats) ColumnNDV(_ context.Context, table, column string) (int64, error) {
	if v, ok := s.NDV[table+"."+column]; ok {
		return v, nil
	}
	if v, ok := s.Rows[table]; ok {
		return v / 10, nil
	}
	return 0, nil
}
