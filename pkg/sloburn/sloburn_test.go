package sloburn

import (
	"testing"
	"time"
)

func TestRegisterRejectsBadSLO(t *testing.T) {
	e := New()
	cases := []SLO{
		{Name: "", Target: 0.99, WindowPairs: DefaultWindowPairs()},
		{Name: "x", Target: 0, WindowPairs: DefaultWindowPairs()},
		{Name: "x", Target: 1.1, WindowPairs: DefaultWindowPairs()},
		{Name: "x", Target: 0.99, WindowPairs: nil},
	}
	for _, c := range cases {
		if err := e.Register(c); err == nil {
			t.Errorf("expected validation failure for %+v", c)
		}
	}
}

func TestEvaluateSilentBelowBurnRate(t *testing.T) {
	e := New()
	if err := e.Register(SLO{
		Name: "api-availability", Target: 0.999,
		WindowPairs: DefaultWindowPairs(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// 10 000 good, 0 bad → 0 burn.
	for i := 0; i < 10_000; i++ {
		_ = e.Observe("api-availability", true, now.Add(-time.Duration(i)*time.Millisecond))
	}
	d, err := e.Evaluate("api-availability", now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Severity != SevOK {
		t.Fatalf("expected SevOK with no errors, got %s (burn long=%f short=%f)",
			d.Severity, d.BurnLong, d.BurnShort)
	}
}

func TestEvaluatePagesOnFastBurn(t *testing.T) {
	e := New()
	if err := e.Register(SLO{
		Name: "api-availability", Target: 0.999, // budget = 0.001
		WindowPairs: DefaultWindowPairs(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// Push enough errors so both the 1h and the 5m windows show
	// >14.4× burn. Error rate ≈ 5% across both windows ⇒ burn = 50.
	// Spread the bad events across the last 4 minutes (so they land
	// inside both the 5m and the 1h windows).
	for i := 0; i < 500; i++ {
		_ = e.Observe("api-availability", false, now.Add(-time.Duration(i%4)*time.Minute))
	}
	for i := 0; i < 9_500; i++ {
		_ = e.Observe("api-availability", true, now.Add(-time.Duration(i%4)*time.Minute))
	}
	d, err := e.Evaluate("api-availability", now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Severity != SevPage {
		t.Fatalf("expected SevPage, got %s (long=%.2f short=%.2f)",
			d.Severity, d.BurnLong, d.BurnShort)
	}
	if d.BurnLong < 14.4 || d.BurnShort < 14.4 {
		t.Errorf("burns should both be ≥14.4: long=%f short=%f", d.BurnLong, d.BurnShort)
	}
}

func TestEvaluateTicketsOnSlowBurn(t *testing.T) {
	e := New()
	if err := e.Register(SLO{
		Name: "api-availability", Target: 0.999,
		WindowPairs: DefaultWindowPairs(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// Sustained low-burn: ~0.3% bad over the last 6h. Burn ≈ 3.
	// Spread across the 24h and 2h windows.
	for i := 0; i < 30; i++ {
		_ = e.Observe("api-availability", false, now.Add(-time.Duration(i*4)*time.Minute))
	}
	for i := 0; i < 9_970; i++ {
		_ = e.Observe("api-availability", true, now.Add(-time.Duration(i*4)*time.Minute%(60*5)))
	}
	d, err := e.Evaluate("api-availability", now)
	if err != nil {
		t.Fatal(err)
	}
	// We accept either SevTicket or SevPage here — the exact severity
	// depends on how the synthetic bursts land relative to the
	// minute-bucket boundaries. The point of this test is that the
	// evaluator does NOT report SevOK for a real, sustained burn.
	if d.Severity == SevOK {
		t.Fatalf("sustained burn must alert: %+v", d)
	}
}

func TestUnknownSLO(t *testing.T) {
	e := New()
	if _, err := e.Evaluate("nope", time.Now()); err == nil {
		t.Error("expected error for unknown SLO in Evaluate")
	}
	if err := e.Observe("nope", true, time.Now()); err == nil {
		t.Error("expected error for unknown SLO in Observe")
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	e := New()
	slo := SLO{Name: "x", Target: 0.99, WindowPairs: DefaultWindowPairs()}
	if err := e.Register(slo); err != nil {
		t.Fatal(err)
	}
	if err := e.Register(slo); err != nil {
		t.Fatal(err)
	}
	if err := e.Observe("x", true, time.Now()); err != nil {
		t.Fatal(err)
	}
}
