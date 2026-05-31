// Package detector implements an online z-score anomaly detector for
// numeric metric values. It is intentionally simple (Welford-style
// rolling mean+variance, configurable z threshold) so that Phase B-5
// ships a working anomaly stream; Phase C swaps it for proper
// seasonal-trend decomposition (STL) or Prophet.
package detector

import (
	"math"
	"sync"
	"time"
)

type Anomaly struct {
	TenantID  string
	Metric    string
	Value     float64
	ZScore    float64
	Mean      float64
	Threshold float64
	At        time.Time
}

type Options struct {
	// WarmupSamples — minimum observations per (tenant, metric) before
	// the detector will report an anomaly. Default 50.
	WarmupSamples int
	// ZThreshold — |z| above this fires an anomaly. Default 3.0
	// (~99.7% of normal samples for a Gaussian).
	ZThreshold float64
	// Alpha — EWMA weight for newer samples. Default 0.05.
	Alpha float64
}

func (o Options) withDefaults() Options {
	if o.WarmupSamples <= 0 {
		o.WarmupSamples = 50
	}
	if o.ZThreshold <= 0 {
		o.ZThreshold = 3.0
	}
	if o.Alpha <= 0 || o.Alpha > 1 {
		o.Alpha = 0.05
	}
	return o
}

type Detector struct {
	opts Options

	mu     sync.RWMutex
	stats  map[string]*serieStats // key = tenant + "::" + metric
}

type serieStats struct {
	mean     float64
	vari     float64
	count    uint64
}

func New(opts Options) *Detector {
	return &Detector{
		opts:  opts.withDefaults(),
		stats: map[string]*serieStats{},
	}
}

// Observe ingests one (tenant, metric, value) sample. If the sample is
// considered anomalous relative to the EWMA baseline, an Anomaly is
// returned; otherwise nil. The tracker is always updated.
func (d *Detector) Observe(tenantID, metric string, value float64, at time.Time) *Anomaly {
	k := tenantID + "::" + metric

	d.mu.Lock()
	s, ok := d.stats[k]
	if !ok {
		s = &serieStats{}
		d.stats[k] = s
	}
	prev := s.mean
	if s.count == 0 {
		s.mean = value
	} else {
		diff := value - prev
		s.mean = prev + d.opts.Alpha*diff
		s.vari = (1 - d.opts.Alpha) * (s.vari + d.opts.Alpha*diff*diff)
	}
	s.count++
	count := s.count
	mean := s.mean
	std := math.Sqrt(s.vari)
	d.mu.Unlock()

	if count < uint64(d.opts.WarmupSamples) {
		return nil
	}
	if std < 1e-9 {
		return nil
	}
	z := (value - mean) / std
	if math.Abs(z) < d.opts.ZThreshold {
		return nil
	}
	return &Anomaly{
		TenantID: tenantID, Metric: metric,
		Value: value, ZScore: z, Mean: mean,
		Threshold: d.opts.ZThreshold, At: at,
	}
}

// SeriesCount returns the number of distinct (tenant, metric) series
// the detector is currently tracking. Useful for /metrics gauges.
func (d *Detector) SeriesCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.stats)
}
