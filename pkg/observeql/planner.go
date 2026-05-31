package observeql

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ─── Allow-listed identifiers ─────────────────────────────────────────────
//
// The planner refuses to emit any column or filter reference that
// isn't on this allow-list. This is the single defence against a
// malicious caller smuggling SQL through an `Ident` token, and it's
// the simplest possible escape hatch for renaming columns later: edit
// this file, not every planner case.

// Columns aligned with the ClickHouse schema in
// pkg/storage/clickhouse/migrations/001_initial_schema.sql. Add only
// columns that physically exist in those tables; aliases (e.g.
// `service` → `service_name`) are not supported in Phase B-3.
var allowedColumnsPerSource = map[string]map[string]bool{
	"metrics": {
		"tenant_id":   true,
		"metric_name": true,
		"timestamp":   true,
		"value":       true,
		"labels":      true,
		"received_at": true,
	},
	"logs": {
		"tenant_id":    true,
		"service_name": true,
		"severity":     true,
		"body":         true,
		"attributes":   true,
		"timestamp":    true,
		"trace_id":     true,
		"span_id":      true,
	},
	"traces": {
		"tenant_id":      true,
		"trace_id":       true,
		"span_id":        true,
		"parent_span_id": true,
		"operation_name": true,
		"service_name":   true,
		"start_time":     true,
		"end_time":       true,
		"duration_ns":    true,
		"attributes":     true,
		"status_code":    true,
	},
}

// timeColumnPerSource is the column the planner uses for the implicit
// SINCE filter and the ORDER BY. Each table has its own:
// metrics + logs use `timestamp`; traces use `start_time`.
var timeColumnPerSource = map[string]string{
	"metrics": "timestamp",
	"logs":    "timestamp",
	"traces":  "start_time",
}

var allowedFunctions = map[string]bool{
	"count": true,
	"avg":   true,
	"sum":   true,
	"min":   true,
	"max":   true,
}

// sourceTables maps the ObserveQL source name to the ClickHouse table.
var sourceTables = map[string]string{
	"metrics": "metrics",
	"logs":    "logs",
	"traces":  "traces",
}

// ─── Plan output ──────────────────────────────────────────────────────────

// Plan is the lowered, parameterised SQL ready for handing to a
// ClickHouse client. tenant_id is always bound to a parameter — never
// interpolated — so a tenant_id with SQL-special characters cannot
// escape the predicate.
type Plan struct {
	SQL      string
	Params   []any // ordered, matches `?` placeholders in SQL
	Source   string
	GroupBy  []string
	Limit    int
	Estimate string // human-readable cost hint for the response payload
}

// PlannerOptions wires per-request constraints.
type PlannerOptions struct {
	TenantID string
	// MaxRowLimit caps the LIMIT clause regardless of what the user
	// asked for. Default 10 000 if zero.
	MaxRowLimit int
	// DefaultSince when SINCE is not provided. Default 1h.
	DefaultSince time.Duration
}

func (o PlannerOptions) withDefaults() PlannerOptions {
	if o.MaxRowLimit <= 0 {
		o.MaxRowLimit = 10_000
	}
	if o.DefaultSince <= 0 {
		o.DefaultSince = time.Hour
	}
	return o
}

// Plan lowers q into ClickHouse SQL.
func PlanQuery(q *Query, opts PlannerOptions) (*Plan, error) {
	opts = opts.withDefaults()
	if opts.TenantID == "" {
		return nil, errors.New("observeql: planner requires tenant id")
	}
	source := q.Source()
	table, ok := sourceTables[source]
	if !ok {
		return nil, fmt.Errorf("observeql: unknown source %q", source)
	}
	allowed := allowedColumnsPerSource[source]

	// ── SELECT ──
	var selectParts []string
	if q.Select.Star {
		selectParts = []string{"*"}
	} else {
		for _, c := range q.Select.Columns {
			s, err := renderColumn(c, allowed)
			if err != nil {
				return nil, err
			}
			selectParts = append(selectParts, s)
		}
	}

	// ── WHERE ──
	whereParts := []string{"tenant_id = ?"}
	params := []any{opts.TenantID}

	if q.Where != nil {
		expr, err := renderExpr(q.Where.Expr, allowed, &params)
		if err != nil {
			return nil, err
		}
		whereParts = append(whereParts, "("+expr+")")
	}

	// ── SINCE ──
	since := opts.DefaultSince
	if q.Since != nil {
		d, err := parseDuration(q.Since.Duration)
		if err != nil {
			return nil, err
		}
		since = d
	}
	timeCol := timeColumnPerSource[source]
	whereParts = append(whereParts, identQuote(timeCol)+" >= ?")
	params = append(params, time.Now().UTC().Add(-since))

	// ── GROUP BY ──
	var groupCols []string
	if q.GroupBy != nil {
		for _, col := range q.GroupBy.Columns {
			if !allowed[col] {
				return nil, fmt.Errorf("observeql: column %q not allowed for source %q", col, source)
			}
			groupCols = append(groupCols, identQuote(col))
		}
	}

	// ── LIMIT ──
	limit := opts.MaxRowLimit
	if q.Limit != nil && q.Limit.N > 0 {
		if q.Limit.N < limit {
			limit = q.Limit.N
		}
	}

	// ── Stitch ──
	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(selectParts, ", "))
	sb.WriteString(" FROM ")
	sb.WriteString(identQuote(table))
	sb.WriteString(" WHERE ")
	sb.WriteString(strings.Join(whereParts, " AND "))
	if len(groupCols) > 0 {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(strings.Join(groupCols, ", "))
	}
	sb.WriteString(" ORDER BY " + identQuote(timeCol) + " DESC LIMIT ?")
	params = append(params, limit)

	return &Plan{
		SQL:      sb.String(),
		Params:   params,
		Source:   source,
		GroupBy:  groupCols,
		Limit:    limit,
		Estimate: fmt.Sprintf("scan: %s since %s, limit %d", source, since, limit),
	}, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

func identQuote(s string) string {
	// ClickHouse uses backticks for identifier quoting. We validated
	// shape via the allow-list above; this is belt and suspenders.
	if !identRe.MatchString(s) {
		panic("observeql: invariant violated: unsafe identifier reached identQuote: " + s)
	}
	return "`" + s + "`"
}

func renderColumn(c *Column, allowed map[string]bool) (string, error) {
	if c.Name != nil {
		if !allowed[*c.Name] {
			return "", fmt.Errorf("observeql: column %q not allowed", *c.Name)
		}
		return identQuote(*c.Name), nil
	}
	if c.Func != nil {
		fn := strings.ToLower(c.Func.Name)
		if !allowedFunctions[fn] {
			return "", fmt.Errorf("observeql: function %q not allowed", fn)
		}
		if c.Func.Star {
			return fn + "(*)", nil
		}
		if c.Func.Arg == nil {
			return "", fmt.Errorf("observeql: function %s requires an argument", fn)
		}
		if !allowed[*c.Func.Arg] {
			return "", fmt.Errorf("observeql: function %s arg %q not allowed", fn, *c.Func.Arg)
		}
		return fn + "(" + identQuote(*c.Func.Arg) + ")", nil
	}
	return "", errors.New("observeql: empty column")
}

func renderExpr(e *Expr, allowed map[string]bool, params *[]any) (string, error) {
	return renderOr(e.Or, allowed, params)
}

func renderOr(o *OrExpr, allowed map[string]bool, params *[]any) (string, error) {
	left, err := renderAnd(o.Left, allowed, params)
	if err != nil {
		return "", err
	}
	for _, r := range o.Right {
		right, err := renderAnd(r, allowed, params)
		if err != nil {
			return "", err
		}
		left = "(" + left + " OR " + right + ")"
	}
	return left, nil
}

func renderAnd(a *AndExpr, allowed map[string]bool, params *[]any) (string, error) {
	left, err := renderCmp(a.Left, allowed, params)
	if err != nil {
		return "", err
	}
	for _, r := range a.Right {
		right, err := renderCmp(r, allowed, params)
		if err != nil {
			return "", err
		}
		left = "(" + left + " AND " + right + ")"
	}
	return left, nil
}

func renderCmp(c *Cmp, allowed map[string]bool, params *[]any) (string, error) {
	if c.Not != nil {
		inner, err := renderCmp(c.Not, allowed, params)
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	}
	if c.Group != nil {
		inner, err := renderExpr(c.Group, allowed, params)
		if err != nil {
			return "", err
		}
		return "(" + inner + ")", nil
	}
	if c.Predicate == nil {
		return "", errors.New("observeql: empty comparison")
	}
	return renderPredicate(c.Predicate, allowed, params)
}

func renderPredicate(p *Predicate, allowed map[string]bool, params *[]any) (string, error) {
	lhs, err := renderValue(p.LHS, allowed, params, true)
	if err != nil {
		return "", err
	}
	rhs, err := renderValue(p.RHS, allowed, params, true)
	if err != nil {
		return "", err
	}
	op := normaliseOp(p.Op)
	return lhs + " " + op + " " + rhs, nil
}

// renderValue emits either an identifier reference (allow-listed) or
// a parameterised literal placeholder.
func renderValue(v *Value, allowed map[string]bool, params *[]any, allowIdent bool) (string, error) {
	switch {
	case v.Ident != nil:
		if !allowIdent {
			return "", fmt.Errorf("observeql: identifier not allowed here")
		}
		// Could be an allow-listed column OR an unquoted string-ish
		// literal. ObserveQL requires quoted strings, so anything
		// that reaches Ident is treated as a column reference.
		if !allowed[*v.Ident] {
			return "", fmt.Errorf("observeql: column %q not allowed", *v.Ident)
		}
		return identQuote(*v.Ident), nil
	case v.String != nil:
		*params = append(*params, *v.String)
		return "?", nil
	case v.Number != nil:
		*params = append(*params, *v.Number)
		return "?", nil
	}
	return "", errors.New("observeql: empty value")
}

func normaliseOp(op string) string {
	// participle joins multi-char operator tokens with no separator;
	// e.g. "!=" arrives as "!=" or "! =" depending on the parser
	// fixture. Normalise whitespace.
	op = strings.ReplaceAll(op, " ", "")
	switch op {
	case "=", "!=", "<", "<=", ">", ">=":
		return op
	}
	return op
}

func parseDuration(d string) (time.Duration, error) {
	// We allow "d" (days) which time.ParseDuration doesn't support.
	if strings.HasSuffix(d, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(d, "d"))
		if err != nil {
			return 0, fmt.Errorf("observeql: invalid duration %q", d)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(d)
}
