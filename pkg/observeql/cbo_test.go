package observeql

import (
	"context"
	"testing"
)

func TestEnumeratePlansAlwaysReturnsRuleBasedFirst(t *testing.T) {
	ast, err := Parse(`SELECT * FROM logs SINCE 1h`)
	if err != nil {
		t.Fatal(err)
	}
	stats := &StubStats{Rows: map[string]int64{"logs": 1_000_000}}
	plans, err := EnumeratePlans(context.Background(), ast,
		PlannerOptions{TenantID: "acme"}, stats)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) < 1 {
		t.Fatalf("expected ≥1 plan")
	}
	if plans[0].Variant != "rule-based" {
		t.Errorf("first variant: %s", plans[0].Variant)
	}
	if plans[0].RowsScanned != 1_000_000 {
		t.Errorf("rows scanned: %d", plans[0].RowsScanned)
	}
}

func TestChooseBestPicksLowestCost(t *testing.T) {
	plans := []*PhysicalPlan{
		{Variant: "expensive", RowsScanned: 10_000_000},
		{Variant: "cheap", RowsScanned: 1_000},
		{Variant: "mid", RowsScanned: 100_000},
	}
	got := ChooseBest(CostModel{}, plans)
	if got.Variant != "cheap" {
		t.Errorf("got %s", got.Variant)
	}
}

func TestStubStatsFallback(t *testing.T) {
	s := &StubStats{Rows: map[string]int64{"logs": 1_000_000}}
	n, _ := s.ColumnNDV(context.Background(), "logs", "unknown_col")
	// Default NDV = rows/10 = 100_000.
	if n != 100_000 {
		t.Errorf("ndv: %d", n)
	}
}

func TestCostModelShuffleWeight(t *testing.T) {
	pp := &PhysicalPlan{RowsScanned: 100, RowsShuffled: 50}
	cheap := CostModel{ShuffleWeight: 0}.Cost(pp)
	if cheap != 100 {
		t.Errorf("cheap: %v", cheap)
	}
	expensive := CostModel{ShuffleWeight: 2}.Cost(pp)
	if expensive != 200 {
		t.Errorf("expensive: %v", expensive)
	}
}
