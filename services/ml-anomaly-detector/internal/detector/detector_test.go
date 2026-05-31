package detector

import (
	"testing"
	"time"
)

func TestDetectorFiresAfterWarmup(t *testing.T) {
	d := New(Options{WarmupSamples: 30, ZThreshold: 3.0})

	// Feed jittered samples around mean 100, std ~6.
	for i := 0; i < 50; i++ {
		v := 90 + float64(i%20) // 90..109
		if got := d.Observe("acme", "rps", v, time.Now()); got != nil {
			t.Fatalf("warmup sample %d should not fire, got %+v", i, got)
		}
	}

	// A 10000 outlier dwarfs the baseline → must fire.
	anom := d.Observe("acme", "rps", 10000, time.Now())
	if anom == nil {
		t.Fatal("expected outlier to fire anomaly")
	}
	if anom.ZScore < 3.0 {
		t.Errorf("z = %f", anom.ZScore)
	}
	if anom.Metric != "rps" || anom.TenantID != "acme" {
		t.Errorf("labels lost: %+v", anom)
	}
}

func TestDetectorIsolatesSeriesAcrossTenants(t *testing.T) {
	d := New(Options{WarmupSamples: 10, ZThreshold: 2.0})

	for i := 0; i < 20; i++ {
		d.Observe("a", "rps", 100, time.Now())
		d.Observe("b", "rps", 5000, time.Now())
	}
	// b's baseline ~5000; 100 from tenant b would be an outlier;
	// 100 from tenant a would not.
	if anom := d.Observe("a", "rps", 100, time.Now()); anom != nil {
		t.Errorf("tenant a should not fire on its own mean: %+v", anom)
	}
	if anom := d.Observe("b", "rps", 100, time.Now()); anom == nil {
		t.Errorf("tenant b should fire on a 100 sample against ~5000 baseline")
	}
	if d.SeriesCount() != 2 {
		t.Errorf("expected 2 series, got %d", d.SeriesCount())
	}
}
