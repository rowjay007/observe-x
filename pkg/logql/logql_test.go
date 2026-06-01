package logql

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

func TestLogQueryBasic(t *testing.T) {
	t.Parallel()
	r, err := Translate(params(`{service="checkout-api"} |= "error"`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !r.IsLogs {
		t.Errorf("expected IsLogs true")
	}
	if !strings.Contains(r.SQL, "service_name = ?") {
		t.Errorf("missing stream selector: %s", r.SQL)
	}
	if !strings.Contains(r.SQL, "positionCaseInsensitive(body, ?)") {
		t.Errorf("missing line filter: %s", r.SQL)
	}
	if r.Args[0] != "checkout-api" || r.Args[1] != "error" {
		t.Errorf("args drift: %v", r.Args)
	}
}

func TestLogQueryAllMatcherKinds(t *testing.T) {
	t.Parallel()
	r, err := Translate(params(`{service="api", severity!="DEBUG", env=~"prod|staging", region!~"us-.*"} |= "x" != "skip" |~ "ERR.+" !~ "ok"`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	must := []string{
		"service_name = ?",
		"severity != ?",
		"match(attributes['env'], ?)",
		"NOT match(attributes['region'], ?)",
		"positionCaseInsensitive(body, ?) > 0",
		"positionCaseInsensitive(body, ?) = 0",
		"match(body, ?)",
		"NOT match(body, ?)",
	}
	for _, m := range must {
		if !strings.Contains(r.SQL, m) {
			t.Errorf("missing %q in:\n%s", m, r.SQL)
		}
	}
}

func TestMetricQueries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		q        string
		want     string
		wantLogs bool
	}{
		{`count_over_time({service="api"}[5m])`, "count() AS v", false},
		{`rate({service="api"}[1m])`, "count() / 30 AS v", false},
		{`bytes_over_time({service="api"}[5m])`, "sum(length(body)) AS v", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.q, func(t *testing.T) {
			r, err := Translate(params(c.q))
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if r.IsLogs != c.wantLogs {
				t.Errorf("IsLogs = %v, want %v", r.IsLogs, c.wantLogs)
			}
			if !strings.Contains(r.SQL, c.want) {
				t.Errorf("missing %q in:\n%s", c.want, r.SQL)
			}
		})
	}
}

func TestRejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		``,                                 // empty
		`{service="api"`,                   // unterminated selector
		`{service=api}`,                    // unquoted value
		`{service="api"} |~ "[bad"`,        // bad regex
		`fooooo({service="api"}[5m])`,      // unknown function
		`topk(5, {service="api"})`,         // unsupported
		`count_over_time({service="api"})`, // missing [duration]
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			_, err := Translate(params(q))
			if err == nil {
				t.Errorf("expected error for %q", q)
			}
		})
	}
}

func TestUnsupportedError(t *testing.T) {
	t.Parallel()
	_, err := Translate(params(`stddev_over_time({service="api"}[5m])`))
	if err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestInjectionSafety(t *testing.T) {
	t.Parallel()
	r, err := Translate(params(`{service="' OR 1=1 --"} |= "; DROP TABLE logs;"`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if strings.Contains(r.SQL, "OR 1=1") || strings.Contains(r.SQL, "DROP TABLE") {
		t.Errorf("injection bytes in SQL:\n%s", r.SQL)
	}
	if r.Args[0] != `' OR 1=1 --` || r.Args[1] != `; DROP TABLE logs;` {
		t.Errorf("malicious values weren't parameterised: %v", r.Args)
	}
}

func TestLabelMappingSpecial(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{service="x"}`:      "service_name = ?",
		`{service_name="x"}`: "service_name = ?",
		`{severity="ERROR"}`: "severity = ?",
		`{level="INFO"}`:     "severity = ?",
		`{trace_id="abc"}`:   "trace_id = ?",
		`{customLabel="x"}`:  "attributes['customLabel']",
	}
	for q, want := range cases {
		q, want := q, want
		t.Run(q, func(t *testing.T) {
			r, err := Translate(params(q))
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if !strings.Contains(r.SQL, want) {
				t.Errorf("missing %q:\n%s", want, r.SQL)
			}
		})
	}
}
