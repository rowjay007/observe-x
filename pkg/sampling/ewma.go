package sampling

import (
	"math"
	"sync"
)

// ewmaTracker keeps an exponentially weighted moving mean + variance
// per key (e.g. per service.name). alpha controls how fast the mean
// adapts: 0.05 ≈ ~20 samples to half-life.
//
// Concurrency: a single tracker guards itself with mu; callers may
// hold many trackers across services without external locking.
type ewmaTracker struct {
	alpha float64

	mu   sync.RWMutex
	mean map[string]float64
	vari map[string]float64 // EWMA of (x-mean)^2
	seen map[string]uint64
}

func newEWMATracker(alpha float64) *ewmaTracker {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.05
	}
	return &ewmaTracker{
		alpha: alpha,
		mean:  map[string]float64{},
		vari:  map[string]float64{},
		seen:  map[string]uint64{},
	}
}

// observe updates the rolling mean and variance for key with x. The
// first observation seeds the mean directly to avoid pulling the
// average to zero for cold-start traffic.
func (t *ewmaTracker) observe(key string, x float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.mean[key]
	if !ok {
		t.mean[key] = x
		t.vari[key] = 0
		t.seen[key] = 1
		return
	}
	diff := x - prev
	t.mean[key] = prev + t.alpha*diff
	t.vari[key] = (1-t.alpha)*(t.vari[key] + t.alpha*diff*diff)
	t.seen[key]++
}

// zscore returns the standardised deviation of x from the rolling
// mean. Returns 0 if the tracker has too few observations to be
// meaningful (n < 30) — we don't want one outlier in a cold tracker
// to dominate the sampling priority.
func (t *ewmaTracker) zscore(key string, x float64) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := t.seen[key]
	if n < 30 {
		return 0
	}
	mean := t.mean[key]
	std := math.Sqrt(t.vari[key])
	if std < 1e-9 {
		return 0
	}
	return (x - mean) / std
}

// snapshot returns a JSON-serialisable copy of the tracker state for
// flushing to a StateStore.
func (t *ewmaTracker) snapshot() map[string]ewmaSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]ewmaSnapshot, len(t.mean))
	for k, m := range t.mean {
		out[k] = ewmaSnapshot{
			Mean:     m,
			Variance: t.vari[k],
			Count:    t.seen[k],
		}
	}
	return out
}

// restore replaces the tracker state from a previously-snapshotted map.
func (t *ewmaTracker) restore(state map[string]ewmaSnapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range state {
		t.mean[k] = s.Mean
		t.vari[k] = s.Variance
		t.seen[k] = s.Count
	}
}

type ewmaSnapshot struct {
	Mean     float64 `json:"m"`
	Variance float64 `json:"v"`
	Count    uint64  `json:"n"`
}
