package observeql

import (
	"strings"
	"testing"
)

func TestParseSimpleSelect(t *testing.T) {
	q, err := Parse(`SELECT * FROM logs WHERE severity = "ERROR" SINCE 1h LIMIT 50`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !q.Select.Star {
		t.Fatal("expected SELECT *")
	}
	if q.From.Source != "logs" {
		t.Fatalf("source = %q", q.From.Source)
	}
	if q.Where == nil || q.Where.Expr == nil {
		t.Fatal("missing WHERE")
	}
	if q.Since == nil || q.Since.Duration != "1h" {
		t.Fatalf("missing SINCE; %+v", q.Since)
	}
	if q.Limit == nil || q.Limit.N != 50 {
		t.Fatalf("limit = %+v", q.Limit)
	}
}

func TestParseAggregateGroupBy(t *testing.T) {
	q, err := Parse(`SELECT service_name, count(*) FROM traces WHERE duration_ns > 1000 GROUP BY service_name`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(q.Select.Columns) != 2 {
		t.Fatalf("want 2 columns, got %d", len(q.Select.Columns))
	}
	if q.GroupBy == nil || q.GroupBy.Columns[0] != "service_name" {
		t.Fatalf("group by = %+v", q.GroupBy)
	}
}

func TestParseCaseInsensitiveKeywords(t *testing.T) {
	cases := []string{
		`select * from logs`,
		`SELECT * FROM LOGS`,
		`SeLeCt * FrOm Logs WhErE severity = "ERROR"`,
	}
	for _, in := range cases {
		if _, err := Parse(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestPlanInjectsTenantId(t *testing.T) {
	q, err := Parse(`SELECT * FROM logs SINCE 5m`)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanQuery(q, PlannerOptions{TenantID: "acme"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(p.SQL, "tenant_id = ?") {
		t.Fatalf("plan missing tenant predicate: %s", p.SQL)
	}
	if got := p.Params[0]; got != "acme" {
		t.Fatalf("first param should be tenant id, got %v", got)
	}
	if !strings.Contains(p.SQL, "ORDER BY `timestamp` DESC") {
		t.Fatalf("plan missing ORDER BY: %s", p.SQL)
	}
}

func TestPlanUsesStartTimeForTraces(t *testing.T) {
	q, _ := Parse(`SELECT trace_id FROM traces SINCE 5m`)
	p, err := PlanQuery(q, PlannerOptions{TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.SQL, "`start_time` >=") || !strings.Contains(p.SQL, "ORDER BY `start_time` DESC") {
		t.Fatalf("traces must use start_time: %s", p.SQL)
	}
}

func TestPlanRejectsUnknownColumn(t *testing.T) {
	q, _ := Parse(`SELECT password FROM logs`)
	if _, err := PlanQuery(q, PlannerOptions{TenantID: "acme"}); err == nil {
		t.Fatal("expected error on disallowed column")
	}
}

func TestPlanRejectsCallerSuppliedTenant(t *testing.T) {
	// Caller cannot override tenant_id via WHERE — that would defeat
	// the safety injection. The planner allows tenant_id as a column
	// in the allow-list (for SELECTs) but the WHERE injection still
	// pins it; an attacker writing `WHERE tenant_id = 'other'` ends
	// up with `(tenant_id = ?) AND (tenant_id = 'other')` which only
	// matches if both are 'acme', i.e. impossible cross-tenant access.
	q, _ := Parse(`SELECT * FROM logs WHERE tenant_id = "evil"`)
	p, err := PlanQuery(q, PlannerOptions{TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.SQL, "tenant_id = ?") {
		t.Fatalf("tenant injection missing: %s", p.SQL)
	}
	// First placeholder MUST still be the trusted tenant id.
	if p.Params[0] != "acme" {
		t.Fatalf("trusted tenant param = %v", p.Params[0])
	}
}

func TestPlanCapsLimit(t *testing.T) {
	q, _ := Parse(`SELECT * FROM logs LIMIT 99999`)
	p, err := PlanQuery(q, PlannerOptions{TenantID: "acme", MaxRowLimit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != 500 {
		t.Fatalf("limit not capped: %d", p.Limit)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	bad := []string{
		``,
		`SELECT`,
		`SELECT FROM logs`,
		`DROP TABLE logs`,
		`SELECT * FROM unknown`,
	}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			t.Errorf("expected parse failure: %q", in)
		}
	}
}

func TestParseDurationDays(t *testing.T) {
	d, err := parseDuration("7d")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hours() != 24*7 {
		t.Fatalf("7d = %v", d)
	}
}
