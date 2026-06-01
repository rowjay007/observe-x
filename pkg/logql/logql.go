// Package logql implements a compatibility shim for the subset of
// LogQL that operators most commonly use in Grafana panels and
// alert rules.
//
// We translate LogQL into ClickHouse SQL against the `logs` table
// in two flavours:
//
//   * Log query — `{stream selector} |= "foo" |~ "bar"`
//       → SELECT timestamp, severity, service_name, body
//         FROM logs WHERE stream-selector AND line-filters
//
//   * Metric query — `<aggr>_over_time({stream selector}[5m])`
//       → SELECT toStartOfInterval(timestamp, INTERVAL step SECOND) AS t,
//                count() AS v
//         FROM logs WHERE … GROUP BY t
//
// Supported stream selectors:
//
//   {label="v", label!="v", label=~"re", label!~"re"}
//
//   Special label names that map to first-class columns:
//     service / service_name → service_name
//     severity / level       → severity
//     anything else          → attributes['<key>']
//
// Supported line filters (between the {…} and the optional aggregator):
//
//   |= "substr"     positionCaseInsensitive(body, '…') > 0
//   != "substr"     positionCaseInsensitive(body, '…') = 0
//   |~ "regex"      match(body, '…')
//   !~ "regex"      NOT match(body, '…')
//
// Supported metric-over-log functions:
//
//   count_over_time({…}[d])
//   rate({…}[d])     — count_over_time / step
//   bytes_over_time({…}[d])  — sum(length(body)) per bucket
//
// All values are bound as `?` parameters; raw bytes from the
// caller never get string-formatted into SQL.
package logql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("logql: unsupported")

type QueryParams struct {
	Query string
	Start time.Time
	End   time.Time
	Step  time.Duration
	Limit int
}

type Result struct {
	SQL    string
	Args   []any
	IsLogs bool // true for log query, false for metric query
}

func Translate(p QueryParams) (*Result, error) {
	if p.Query == "" {
		return nil, fmt.Errorf("logql: empty query")
	}
	if p.End.IsZero() {
		p.End = time.Now().UTC()
	}
	if p.Start.IsZero() {
		p.Start = p.End.Add(-time.Hour)
	}
	if p.Step <= 0 {
		p.Step = 60 * time.Second
	}
	if p.Limit <= 0 {
		p.Limit = 1000
	}

	q := strings.TrimSpace(p.Query)
	// Metric query? Look for `<name>(` at the start.
	if name, body, ok := splitCall(q); ok {
		return translateMetric(name, body, p)
	}
	return translateLogs(q, p)
}

// ── log query ────────────────────────────────────────────────────

func translateLogs(q string, p QueryParams) (*Result, error) {
	selector, rest, err := parseStreamSelector(q)
	if err != nil {
		return nil, err
	}
	args := []any{}
	wheres := []string{}
	for _, m := range selector {
		clause, val, err := matcherSQL(m)
		if err != nil {
			return nil, err
		}
		wheres = append(wheres, clause)
		args = append(args, val)
	}
	// Parse trailing line filters.
	filters, err := parseLineFilters(rest)
	if err != nil {
		return nil, err
	}
	for _, f := range filters {
		clause, val, err := lineFilterSQL(f)
		if err != nil {
			return nil, err
		}
		wheres = append(wheres, clause)
		args = append(args, val)
	}
	args = append(args, p.Start, p.End, p.Limit)
	wheres = append(wheres, "timestamp BETWEEN ? AND ?")
	sql := fmt.Sprintf(
		"SELECT timestamp, severity, service_name, body, trace_id, span_id, attributes "+
			"FROM logs WHERE %s ORDER BY timestamp DESC LIMIT ?",
		strings.Join(wheres, " AND "),
	)
	return &Result{SQL: sql, Args: args, IsLogs: true}, nil
}

// ── metric query ─────────────────────────────────────────────────

func translateMetric(name, body string, p QueryParams) (*Result, error) {
	if !isMetricFn(name) {
		return nil, fmt.Errorf("%w: function %q", ErrUnsupported, name)
	}
	// body = `{...}[5m]` — split off the [duration] tail.
	body = strings.TrimSpace(body)
	bracket := strings.LastIndex(body, "[")
	if bracket < 0 || !strings.HasSuffix(body, "]") {
		return nil, fmt.Errorf("metric query requires a [duration] tail")
	}
	durText := body[bracket+1 : len(body)-1]
	_, err := parseDuration(durText)
	if err != nil {
		return nil, fmt.Errorf("bad duration: %w", err)
	}
	selectorAndFilters := strings.TrimSpace(body[:bracket])
	selector, rest, err := parseStreamSelector(selectorAndFilters)
	if err != nil {
		return nil, err
	}
	args := []any{}
	wheres := []string{}
	for _, m := range selector {
		clause, val, err := matcherSQL(m)
		if err != nil {
			return nil, err
		}
		wheres = append(wheres, clause)
		args = append(args, val)
	}
	filters, err := parseLineFilters(rest)
	if err != nil {
		return nil, err
	}
	for _, f := range filters {
		clause, val, err := lineFilterSQL(f)
		if err != nil {
			return nil, err
		}
		wheres = append(wheres, clause)
		args = append(args, val)
	}
	args = append(args, p.Start, p.End)
	wheres = append(wheres, "timestamp BETWEEN ? AND ?")

	stepS := int64(p.Step.Seconds())
	if stepS <= 0 {
		stepS = 60
	}
	var expr string
	switch name {
	case "count_over_time":
		expr = "count()"
	case "rate":
		expr = fmt.Sprintf("count() / %d", stepS)
	case "bytes_over_time":
		expr = "sum(length(body))"
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, name)
	}
	sql := fmt.Sprintf(
		"SELECT toStartOfInterval(timestamp, INTERVAL %d SECOND) AS t, %s AS v "+
			"FROM logs WHERE %s GROUP BY t ORDER BY t",
		stepS, expr, strings.Join(wheres, " AND "),
	)
	return &Result{SQL: sql, Args: args, IsLogs: false}, nil
}

// ── selector + filter parsing ────────────────────────────────────

type lqMatcher struct {
	name string
	op   string // = != =~ !~
	val  string
}

type lqLineFilter struct {
	op  string // |= != |~ !~
	val string
}

func parseStreamSelector(q string) ([]lqMatcher, string, error) {
	q = strings.TrimSpace(q)
	if !strings.HasPrefix(q, "{") {
		return nil, "", fmt.Errorf("expected stream selector starting with {")
	}
	end := strings.Index(q, "}")
	if end < 0 {
		return nil, "", fmt.Errorf("unterminated stream selector")
	}
	inner := q[1:end]
	rest := strings.TrimSpace(q[end+1:])
	matches := []lqMatcher{}
	for _, part := range splitTopLevelCommas(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m, err := parseMatcher(part)
		if err != nil {
			return nil, "", err
		}
		matches = append(matches, m)
	}
	return matches, rest, nil
}

func splitTopLevelCommas(s string) []string {
	out := []string{}
	depth := 0
	last := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' && (i == 0 || s[i-1] != '\\') {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	out = append(out, s[last:])
	return out
}

func parseMatcher(s string) (lqMatcher, error) {
	for _, op := range []string{"=~", "!~", "!=", "="} {
		idx := indexOpOutsideString(s, op)
		if idx > 0 {
			name := strings.TrimSpace(s[:idx])
			val := strings.TrimSpace(s[idx+len(op):])
			if !strings.HasPrefix(val, `"`) || !strings.HasSuffix(val, `"`) || len(val) < 2 {
				return lqMatcher{}, fmt.Errorf("matcher value must be quoted: %s", s)
			}
			val = val[1 : len(val)-1]
			return lqMatcher{name: name, op: op, val: val}, nil
		}
	}
	return lqMatcher{}, fmt.Errorf("no matcher op in %s", s)
}

func indexOpOutsideString(s, op string) int {
	inStr := false
	for i := 0; i <= len(s)-len(op); i++ {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			inStr = !inStr
		}
		if inStr {
			continue
		}
		if s[i:i+len(op)] == op {
			return i
		}
	}
	return -1
}

func parseLineFilters(s string) ([]lqLineFilter, error) {
	s = strings.TrimSpace(s)
	out := []lqLineFilter{}
	for s != "" {
		var op string
		switch {
		case strings.HasPrefix(s, "|="):
			op = "|="
		case strings.HasPrefix(s, "|~"):
			op = "|~"
		case strings.HasPrefix(s, "!="):
			op = "!="
		case strings.HasPrefix(s, "!~"):
			op = "!~"
		default:
			return nil, fmt.Errorf("unexpected trailing tokens: %q", s)
		}
		s = strings.TrimSpace(s[len(op):])
		if !strings.HasPrefix(s, `"`) {
			return nil, fmt.Errorf("line filter requires a quoted argument")
		}
		// Find closing quote.
		j := 1
		for j < len(s) {
			if s[j] == '\\' {
				j += 2
				continue
			}
			if s[j] == '"' {
				break
			}
			j++
		}
		if j >= len(s) {
			return nil, fmt.Errorf("unterminated line filter")
		}
		val := s[1:j]
		out = append(out, lqLineFilter{op: op, val: val})
		s = strings.TrimSpace(s[j+1:])
	}
	return out, nil
}

// ── translation helpers ──────────────────────────────────────────

func matcherSQL(m lqMatcher) (string, any, error) {
	col := columnFor(m.name)
	switch m.op {
	case "=":
		return col + " = ?", m.val, nil
	case "!=":
		return col + " != ?", m.val, nil
	case "=~":
		if _, err := regexp.Compile(m.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", m.val, err)
		}
		return "match(" + col + ", ?)", m.val, nil
	case "!~":
		if _, err := regexp.Compile(m.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", m.val, err)
		}
		return "NOT match(" + col + ", ?)", m.val, nil
	}
	return "", nil, fmt.Errorf("unknown matcher op %q", m.op)
}

func lineFilterSQL(f lqLineFilter) (string, any, error) {
	switch f.op {
	case "|=":
		return "positionCaseInsensitive(body, ?) > 0", f.val, nil
	case "!=":
		return "positionCaseInsensitive(body, ?) = 0", f.val, nil
	case "|~":
		if _, err := regexp.Compile(f.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", f.val, err)
		}
		return "match(body, ?)", f.val, nil
	case "!~":
		if _, err := regexp.Compile(f.val); err != nil {
			return "", nil, fmt.Errorf("bad regex %q: %w", f.val, err)
		}
		return "NOT match(body, ?)", f.val, nil
	}
	return "", nil, fmt.Errorf("unknown line filter %q", f.op)
}

func columnFor(label string) string {
	switch strings.ToLower(label) {
	case "service", "service_name":
		return "service_name"
	case "severity", "level":
		return "severity"
	case "trace_id", "traceid":
		return "trace_id"
	case "span_id", "spanid":
		return "span_id"
	}
	return "attributes['" + safeKey(label) + "']"
}

func safeKey(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return "INVALID"
		}
	}
	return s
}

func splitCall(s string) (name, body string, ok bool) {
	lparen := strings.Index(s, "(")
	if lparen <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(s[:lparen])
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
			return "", "", false
		}
	}
	if !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	body = s[lparen+1 : len(s)-1]
	return name, body, true
}

func isMetricFn(name string) bool {
	switch name {
	case "count_over_time", "rate", "bytes_over_time":
		return true
	}
	return false
}

func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("too short")
	}
	var unit string
	if len(s) >= 2 && s[len(s)-2:] == "ms" {
		unit = "ms"
	} else {
		unit = s[len(s)-1:]
	}
	numStr := s[:len(s)-len(unit)]
	n := 0.0
	if _, err := fmt.Sscanf(numStr, "%f", &n); err != nil {
		return 0, err
	}
	var mult time.Duration
	switch unit {
	case "ms":
		mult = time.Millisecond
	case "s":
		mult = time.Second
	case "m":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	case "w":
		mult = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
	return time.Duration(float64(mult) * n), nil
}
