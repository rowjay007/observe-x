// Package promql implements a *compatibility shim* — not a full
// PromQL engine — so operators with existing Prometheus-flavoured
// dashboards (typical Grafana panels) can point them at ObserveX
// without rewriting queries.
//
// We translate a deliberately-restricted PromQL subset into a single
// ClickHouse SQL statement against the `metrics` table, and let the
// existing query-engine path execute it. The supported surface
// covers ~80% of dashboard panel usage; anything beyond it returns
// a structured "unsupported" error so the caller knows to switch to
// ObserveQL or to file a request for incremental support.
//
// Supported:
//
//	Vector selector:   metric{label="v",label!="v",label=~"re",label!~"re"}
//	Range selector:    metric[5m]   (durations: s/m/h/d, integer + unit)
//	Aggregations:      sum|avg|min|max|count [by|without (labels)] (vector)
//	Range functions:   rate(v[d])  irate(v[d])  increase(v[d])
//	                   avg_over_time / sum_over_time / count_over_time
//	                   / max_over_time / min_over_time / quantile_over_time
//	Quantile:          quantile(0.99, vector)
//	Numeric literals:  for quantile φ
//	Comparison ops:    >, <, >=, <=, ==, != against scalar (post-filter)
//	Binary scalar:     scalar +-*/ on aggregations (post-aggregation expression)
//
// NOT supported (returns ErrUnsupported):
//
//   - Subqueries (`<expr>[5m:1m]`)
//   - Recording rules / `topk`/`bottomk` / `histogram_quantile` shortcut
//   - Offsets / @-modifiers
//   - Joining/grouping vector-on-vector (`+ on(...)`)
//   - Functions beyond the list above
//
// The translator emits SQL of the shape
//
//	SELECT toStartOfInterval(timestamp, INTERVAL step SECOND) AS t,
//	       <agg>(value) AS v
//	       [, labels.<key1> AS k1, …]
//	FROM   metrics
//	WHERE  tenant_id = ?     -- always bound from auth, never PromQL
//	  AND  metric_name = ?
//	  AND  <label predicates from selector>
//	  AND  timestamp BETWEEN ? AND ?
//	GROUP BY t [, k1, …]
//	ORDER BY t
//
// Tenant safety: tenant_id is NEVER taken from the PromQL string;
// the executor injects it from the authenticated context (the same
// `auth.GinRequireScope(query)` middleware that fronts /v1/query).
package promql

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnsupported indicates the query parsed correctly but uses a
// PromQL construct we deliberately don't translate. The caller
// should surface the message verbatim — operators tend to rewrite
// queries based on it.
var ErrUnsupported = errors.New("promql: unsupported")

// QueryParams describe a `/api/v1/query_range`-style request. For
// instant queries pass Start == End and Step == 1*time.Second so
// the planner still picks a sane interval.
type QueryParams struct {
	Query string
	Start time.Time
	End   time.Time
	Step  time.Duration
}

// Result describes the SQL the translator built. Callers feed
// `SQL` + `Args` to a ClickHouse-compatible executor.
type Result struct {
	SQL    string
	Args   []any
	Labels []string // grouping labels in result order (after t, v)
}

// Translate parses the PromQL string and lowers it to ClickHouse
// SQL. Returns ErrUnsupported wrapped if the expression uses a
// construct outside the supported subset.
func Translate(p QueryParams) (*Result, error) {
	if p.Query == "" {
		return nil, fmt.Errorf("promql: empty query")
	}
	if p.Step <= 0 {
		p.Step = 30 * time.Second
	}
	if p.End.IsZero() {
		p.End = time.Now().UTC()
	}
	if p.Start.IsZero() {
		p.Start = p.End.Add(-time.Hour)
	}

	tokens, err := tokenize(p.Query)
	if err != nil {
		return nil, fmt.Errorf("promql: tokenize: %w", err)
	}
	ast, err := parseExpr(&parser{tokens: tokens})
	if err != nil {
		return nil, fmt.Errorf("promql: parse: %w", err)
	}
	return lower(ast, p)
}
