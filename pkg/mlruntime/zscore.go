package mlruntime

import (
	"context"
	"math"
	"sync"
)

// ZScorePredictor is the default in-process Welford-style EWMA
// detector. It's the same algorithm `services/ml-anomaly-detector/
// internal/detector` ships, exposed through the Predictor seam so the
// detector code and the new ONNX adapter live behind one consumer
// surface.
type ZScorePredictor struct {
	opts  ZScoreOptions
	mu    sync.Mutex
	stats map[string]*zEntry
}

type zEntry struct {
	mean  float64
	vari  float64
	count uint64
}

type ZScoreOptions struct {
	// WarmupSamples gates anomaly reporting until enough history is
	// accumulated. Default 50.
	WarmupSamples int
	// ZThreshold — |z| above this fires an anomaly. Default 3.0
	// (~99.7% for Gaussian).
	ZThreshold float64
	// Alpha — EWMA weight for newer samples. Default 0.05.
	Alpha float64
}

func (o ZScoreOptions) withDefaults() ZScoreOptions {
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

func NewZScorePredictor(opts ZScoreOptions) *ZScorePredictor {
	return &ZScorePredictor{
		opts:  opts.withDefaults(),
		stats: map[string]*zEntry{},
	}
}

func (p *ZScorePredictor) Name() string { return "zscore-ewma" }

func (p *ZScorePredictor) Observe(_ context.Context, s Sample) (Decision, error) {
	key := s.TenantID + "::" + s.Metric

	p.mu.Lock()
	e, ok := p.stats[key]
	if !ok {
		e = &zEntry{}
		p.stats[key] = e
	}
	prev := e.mean
	if e.count == 0 {
		e.mean = s.Value
	} else {
		diff := s.Value - prev
		e.mean = prev + p.opts.Alpha*diff
		e.vari = (1 - p.opts.Alpha) * (e.vari + p.opts.Alpha*diff*diff)
	}
	e.count++
	count := e.count
	mean := e.mean
	std := math.Sqrt(e.vari)
	p.mu.Unlock()

	d := Decision{Baseline: mean, Threshold: p.opts.ZThreshold}
	if count < uint64(p.opts.WarmupSamples) || std < 1e-9 {
		return d, nil
	}
	z := (s.Value - mean) / std
	d.Score = z
	if math.Abs(z) >= p.opts.ZThreshold {
		d.Anomaly = true
	}
	return d, nil
}

func (p *ZScorePredictor) Close() error { return nil }

// SeriesCount reports the number of distinct (tenant, metric) series
// currently tracked. Mirrors the existing detector's accessor.
func (p *ZScorePredictor) SeriesCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stats)
}
