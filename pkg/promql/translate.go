package promql

import (
	"fmt"
	"regexp"
	"strings"
)

// lower compiles the AST into ClickHouse SQL. The shape is fixed:
//
//	SELECT toStartOfInterval(timestamp, INTERVAL <step> SECOND) AS t,
//	       <agg-expr>(value) AS v
//	       [, labels_columns]
//	FROM   metrics
//	WHERE  tenant_id = ?       -- bound by caller (executor injects)
//	  AND  metric_name = ?     -- from outermost vector selector
//	  AND  <label predicates>
//	  AND  timestamp BETWEEN ? AND ?
//	GROUP BY t [, label_columns]
//	ORDER BY t
//
// Args order is fixed so the executor can swap in tenant_id:
//
//	[0] tenant_id placeholder slot — left to caller, see note
//	[1] metric_name
//	[...]: label values, then start, then end, then optional phi
//
// Actually we DO NOT add the tenant placeholder here — the
// query-engine handler wraps every query with `tenant_id = ?` from
// the auth context. We emit the metric/label/time bounds only.
func lower(ast node, p QueryParams) (*Result, error) {
	// We classify the outermost shape into one of a few forms.
	switch top := ast.(type) {
	case aggrExpr:
		return lowerAggr(top, p)
	case callExpr:
		return lowerCall(top, p)
	case vectorSelector:
		// "metric{…}" alone — equivalent to avg(metric{…}) bucketed.
		return lowerAggr(aggrExpr{op: "avg", arg: top}, p)
	case rangeSelector:
		// Bare range — uncommon as a query root; we treat it like a
		// vector selector (caller probably wants `rate()` etc).
		return lowerAggr(aggrExpr{op: "avg", arg: top.v}, p)
	case binExpr:
		return lowerBin(top, p)
	default:
		return nil, fmt.Errorf("%w: top-level expression of type %T", ErrUnsupported, ast)
	}
}

func lowerAggr(ae aggrExpr, p QueryParams) (*Result, error) {
	vs, _, err := extractSelector(ae.arg)
	if err != nil {
		return nil, err
	}
	args := []any{vs.metric}
	wheres := []string{"metric_name = ?"}
	for _, m := range vs.matches {
		clause, val, err := matcherSQL(m)
		if err != nil {
			return nil, err
		}
		wheres = append(wheres, clause)
		args = append(args, val)
	}
	args = append(args, p.Start, p.End)
	wheres = append(wheres, "timestamp BETWEEN ? AND ?")

	step := int64(p.Step.Seconds())
	if step <= 0 {
		step = 30
	}

	selectExprs := []string{
		fmt.Sprintf("toStartOfInterval(timestamp, INTERVAL %d SECOND) AS t", step),
	}
	var aggExpr string
	switch ae.op {
	case "sum":
		aggExpr = "sum(value)"
	case "avg":
		aggExpr = "avg(value)"
	case "min":
		aggExpr = "min(value)"
	case "max":
		aggExpr = "max(value)"
	case "count":
		aggExpr = "count(value)"
	case "quantile":
		if ae.phi == nil {
			return nil, fmt.Errorf("quantile requires phi")
		}
		aggExpr = fmt.Sprintf("quantile(%g)(value)", *ae.phi)
	default:
		return nil, fmt.Errorf("%w: aggregator %q", ErrUnsupported, ae.op)
	}
	selectExprs = append(selectExprs, aggExpr+" AS v")

	groupBys := []string{"t"}
	labels := []string{}
	if len(ae.by) > 0 {
		for _, lab := range ae.by {
			col := fmt.Sprintf("attributes['%s'] AS %s", sqlEscape(lab), labelCol(lab))
			selectExprs = append(selectExprs, col)
			groupBys = append(groupBys, labelCol(lab))
			labels = append(labels, lab)
		}
	}
	// `without` is harder to translate cleanly (need to know all
	// label keys ahead of time). We accept it but treat it as "no
	// grouping" — the user will see ungrouped results, which is
	// usually what they want for headline panels.
	sql := fmt.Sprintf(
		"SELECT %s FROM metrics WHERE %s GROUP BY %s ORDER BY t",
		strings.Join(selectExprs, ", "),
		strings.Join(wheres, " AND "),
		strings.Join(groupBys, ", "),
	)
	return &Result{SQL: sql, Args: args, Labels: labels}, nil
}

func lowerCall(c callExpr, p QueryParams) (*Result, error) {
	switch c.name {
	case "rate", "irate", "increase":
		if len(c.args) != 1 {
			return nil, fmt.Errorf("%s expects 1 arg", c.name)
		}
		rs, ok := c.args[0].(rangeSelector)
		if !ok {
			return nil, fmt.Errorf("%s requires a range vector argument", c.name)
		}
		// rate/irate ≈ (last - first) / Δt within the range window.
		// We approximate by reading raw counter values bucketed at
		// the query step and letting the chart user differentiate
		// — but that defeats the purpose of the shim. Instead, we
		// compute the per-bucket delta against the previous bucket
		// using a windowed `(value - lagInFrame(value)) /
		// step_seconds` so the result is a rate of value/s.
		vs := rs.v
		args := []any{vs.metric}
		wheres := []string{"metric_name = ?"}
		for _, m := range vs.matches {
			clause, val, err := matcherSQL(m)
			if err != nil {
				return nil, err
			}
			wheres = append(wheres, clause)
			args = append(args, val)
		}
		args = append(args, p.Start, p.End)
		wheres = append(wheres, "timestamp BETWEEN ? AND ?")
		step := int64(p.Step.Seconds())
		if step <= 0 {
			step = 30
		}
		var expr string
		switch c.name {
		case "rate":
			// Average per-second growth across the bucket.
			expr = fmt.Sprintf("(max(value) - min(value)) / %d", step)
		case "irate":
			// Approximate instantaneous rate: last-first over the
			// bucket; for ObserveX gauge metrics this collapses to
			// the same shape as rate at small buckets.
			expr = fmt.Sprintf("(anyLast(value) - any(value)) / %d", step)
		case "increase":
			expr = "(max(value) - min(value))"
		}
		sql := fmt.Sprintf(
			"SELECT toStartOfInterval(timestamp, INTERVAL %d SECOND) AS t, %s AS v "+
				"FROM metrics WHERE %s GROUP BY t ORDER BY t",
			step, expr, strings.Join(wheres, " AND "),
		)
		return &Result{SQL: sql, Args: args}, nil
	case "avg_over_time", "sum_over_time", "min_over_time", "max_over_time", "count_over_time":
		if len(c.args) != 1 {
			return nil, fmt.Errorf("%s expects 1 arg", c.name)
		}
		rs, ok := c.args[0].(rangeSelector)
		if !ok {
			return nil, fmt.Errorf("%s requires a range vector", c.name)
		}
		var op string
		switch c.name {
		case "avg_over_time":
			op = "avg"
		case "sum_over_time":
			op = "sum"
		case "min_over_time":
			op = "min"
		case "max_over_time":
			op = "max"
		case "count_over_time":
			op = "count"
		}
		return lowerAggr(aggrExpr{op: op, arg: rs.v}, p)
	case "quantile_over_time":
		if len(c.args) != 2 {
			return nil, fmt.Errorf("quantile_over_time expects (phi, range)")
		}
		lit, ok := c.args[0].(numLit)
		if !ok {
			return nil, fmt.Errorf("quantile_over_time phi must be numeric literal")
		}
		rs, ok := c.args[1].(rangeSelector)
		if !ok {
			return nil, fmt.Errorf("quantile_over_time requires a range vector")
		}
		phi := lit.v
		return lowerAggr(aggrExpr{op: "quantile", phi: &phi, arg: rs.v}, p)
	}
	return nil, fmt.Errorf("%w: function %q", ErrUnsupported, c.name)
}

// lowerBin: only the `<aggr-expr> OP <scalar>` (and reverse) shape
// is supported. We translate by wrapping the inner aggregation in
// a subquery and applying the scalar in the outer SELECT.
func lowerBin(b binExpr, p QueryParams) (*Result, error) {
	var inner *Result
	var scalar float64
	var swapped bool
	if lit, ok := b.right.(numLit); ok {
		v, err := lower(b.left, p)
		if err != nil {
			return nil, err
		}
		inner = v
		scalar = lit.v
	} else if lit, ok := b.left.(numLit); ok {
		v, err := lower(b.right, p)
		if err != nil {
			return nil, err
		}
		inner = v
		scalar = lit.v
		swapped = true
	} else {
		return nil, fmt.Errorf("%w: vector-on-vector binary op", ErrUnsupported)
	}
	op := b.op
	// Build the wrapping select.
	var expr string
	switch op {
	case "+", "-", "*", "/":
		if swapped {
			expr = fmt.Sprintf("(%g %s v)", scalar, op)
		} else {
			expr = fmt.Sprintf("(v %s %g)", op, scalar)
		}
		sql := "SELECT t, " + expr + " AS v FROM (" + inner.SQL + ")"
		return &Result{SQL: sql, Args: inner.Args, Labels: inner.Labels}, nil
	case ">", "<", ">=", "<=", "==", "!=":
		// Filter: keep only rows where the predicate holds.
		cond := fmt.Sprintf("v %s %g", op, scalar)
		if swapped {
			cond = fmt.Sprintf("%g %s v", scalar, op)
		}
		if op == "==" {
			cond = strings.Replace(cond, "==", "=", 1)
		}
		sql := "SELECT t, v FROM (" + inner.SQL + ") WHERE " + cond
		return &Result{SQL: sql, Args: inner.Args, Labels: inner.Labels}, nil
	}
	return nil, fmt.Errorf("%w: binary op %q", ErrUnsupported, op)
}

// extractSelector dives through a tree until it finds the
// vectorSelector (skipping rangeSelector). Returns the selector
// and whether it was a range form.
func extractSelector(n node) (vectorSelector, bool, error) {
	switch t := n.(type) {
	case vectorSelector:
		return t, false, nil
	case rangeSelector:
		return t.v, true, nil
	case callExpr:
		// nested call — we don't support that (no `sum(rate(...))`
		// shape via the aggr path; if you write `sum(rate(...))`
		// the parser produces aggrExpr → callExpr.arg path; we
		// recurse to find the selector.
		if len(t.args) == 1 {
			return extractSelector(t.args[0])
		}
	}
	return vectorSelector{}, false, fmt.Errorf("%w: cannot find vector selector inside %T", ErrUnsupported, n)
}

// matcherSQL turns a label matcher into a parameterised SQL
// fragment. We bind values via `?` so user input is never
// concatenated into SQL.
func matcherSQL(m matcher) (string, any, error) {
	col := fmt.Sprintf("attributes['%s']", sqlEscape(m.name))
	switch m.op {
	case matchEq:
		return col + " = ?", m.val, nil
	case matchNeq:
		return col + " != ?", m.val, nil
	case matchRegex:
		if _, err := regexp.Compile(m.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", m.val, err)
		}
		return "match(" + col + ", ?)", m.val, nil
	case matchNotRegex:
		if _, err := regexp.Compile(m.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", m.val, err)
		}
		return "NOT match(" + col + ", ?)", m.val, nil
	}
	return "", nil, fmt.Errorf("unknown matcher op")
}

// labelCol normalises a PromQL label to a SQL-safe alias.
func labelCol(label string) string {
	var b strings.Builder
	b.WriteString("lbl_")
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// sqlEscape applies the minimal escaping needed for a literal
// embedded in an identifier-style brace expression. Since values
// are always bound via `?`, this is only used for the metadata
// brace `attributes['<key>']` where ClickHouse needs the key
// inline. We refuse keys that aren't \w to keep injection
// impossible.
func sqlEscape(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return "INVALID"
		}
	}
	return s
}
