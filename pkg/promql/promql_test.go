package promql

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func params(q string) QueryParams {
	return QueryParams{
		Query: q,
		Start: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Step:  30 * time.Second,
	}
}

func TestTranslateAggregations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		query    string
		wantSQL  string // substring assertions
		wantArgs []any  // first metric name + label values (start/end vary)
	}{
		{
			"bare metric → avg",
			`rps`,
			"avg(value) AS v",
			[]any{"rps"},
		},
		{
			"sum by",
			`sum(rps{service="checkout"}) by (service)`,
			"sum(value) AS v",
			[]any{"rps", "checkout"},
		},
		{
			"max with negative matcher",
			`max(latency_ms{tier!="canary"})`,
			"max(value) AS v",
			[]any{"latency_ms", "canary"},
		},
		{
			"quantile literal phi",
			`quantile(0.99, latency_ms{service="api"})`,
			"quantile(0.99)(value) AS v",
			[]any{"latency_ms", "api"},
		},
		{
			"regex matcher",
			`avg(rps{service=~"checkout|api"})`,
			"match(attributes['service'], ?)",
			[]any{"rps", "checkout|api"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r, err := Translate(params(c.query))
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if !strings.Contains(r.SQL, c.wantSQL) {
				t.Errorf("SQL missing %q:\n%s", c.wantSQL, r.SQL)
			}
			for i, want := range c.wantArgs {
				if i >= len(r.Args) {
					t.Fatalf("args truncated at %d: %v", i, r.Args)
				}
				if r.Args[i] != want {
					t.Errorf("arg[%d] = %v, want %v", i, r.Args[i], want)
				}
			}
			// Time bounds are always the last two args (before any
			// post-translate additions for scalar bin-ops).
			if len(r.Args) < 2 {
				t.Fatalf("expected ≥2 args, got %v", r.Args)
			}
		})
	}
}

func TestTranslateRangeFunctions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  string
	}{
		{`rate(rps[5m])`, "(max(value) - min(value)) /"},
		{`irate(rps[1m])`, "anyLast(value) - any(value)"},
		{`increase(rps[1h])`, "(max(value) - min(value))"},
		{`avg_over_time(rps[5m])`, "avg(value) AS v"},
		{`sum_over_time(rps[5m])`, "sum(value) AS v"},
		{`max_over_time(rps[5m])`, "max(value) AS v"},
		{`count_over_time(rps[5m])`, "count(value) AS v"},
		{`quantile_over_time(0.95, latency_ms[5m])`, "quantile(0.95)(value) AS v"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.query, func(t *testing.T) {
			r, err := Translate(params(c.query))
			if err != nil {
				t.Fatalf("translate %s: %v", c.query, err)
			}
			if !strings.Contains(r.SQL, c.want) {
				t.Errorf("SQL missing %q:\n%s", c.want, r.SQL)
			}
		})
	}
}

func TestTranslateScalarBinaryOps(t *testing.T) {
	t.Parallel()
	r, err := Translate(params(`avg(rps{service="api"}) > 100`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(r.SQL, "WHERE v > 100") {
		t.Errorf("expected WHERE v > 100, got:\n%s", r.SQL)
	}

	r2, err := Translate(params(`sum(rps) * 1000`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(r2.SQL, "(v * 1000)") {
		t.Errorf("expected (v * 1000), got:\n%s", r2.SQL)
	}
}

func TestTranslateRejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		`topk(5, rps)`,            // unsupported aggregator
		`histogram_quantile(0.95, sum(rate(latency_bucket[5m])))`, // function not in subset
		`up + down`, // vector-on-vector binop
		``,          // empty
		`rate()`,    // arity error
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			_, err := Translate(params(q))
			if err == nil {
				t.Fatalf("expected error for %q, got nil", q)
			}
		})
	}
}

func TestUnsupportedError(t *testing.T) {
	t.Parallel()
	_, err := Translate(params(`topk(5, rps)`))
	if err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"30s":  30 * time.Second,
		"5m":   5 * time.Minute,
		"1h":   time.Hour,
		"1d":   24 * time.Hour,
		"1w":   7 * 24 * time.Hour,
		"500ms": 500 * time.Millisecond,
	}
	for s, want := range cases {
		got, err := parseDuration(s)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", s, got, want)
		}
	}
}

func TestLabelMatcherSafety(t *testing.T) {
	t.Parallel()
	// Attempt SQL injection via a label name. The translator MUST
	// either reject or escape so the resulting SQL is harmless.
	r, err := Translate(params(`rps{evil="' OR 1=1 --"}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if strings.Contains(r.SQL, "OR 1=1") {
		t.Errorf("injection bytes leaked into SQL:\n%s", r.SQL)
	}
	// The malicious value should be bound as a parameter, not
	// inlined.
	found := false
	for _, a := range r.Args {
		if a == `' OR 1=1 --` {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("malicious value missing from Args (so it must have been inlined?): %v", r.Args)
	}
}
