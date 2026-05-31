// Package cep — Complex Event Processing rules over per-tenant signals.
//
// Phase B-4 replaces the Phase A toy rule (which divided a running
// counter by the magic number 100 and called the result a rate) with
// a proper per-service sliding-window rate calculator. Two production
// rules ship: HighErrorRateRule and HighLatencyRule.
//
// Design choice — minute buckets, configurable window:
//
//   Each service tracks counts in N×1-minute buckets that form a
//   ring. The "rate" at any instant is the sum across all live
//   buckets divided by their span in seconds. That's O(N) per
//   evaluation with N≤window/bucketSize; at default 5-minute window
//   it's 5 adds. The rule fires on threshold-crossing edges (not on
//   every signal above threshold) to avoid alert storms; once an
//   event has fired for (tenant, service), no new event fires until
//   the rate dips below threshold first.
package cep

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
)

type EventType string

const (
	HighErrorRate EventType = "HIGH_ERROR_RATE"
	HighLatency   EventType = "HIGH_LATENCY"
)

type Event struct {
	Type      EventType
	TenantID  string
	Timestamp time.Time
	Data      map[string]any
}

type Rule interface {
	Evaluate(sig signal.Signal) *Event
}

// ─── sliding window primitive ─────────────────────────────────────────────

// slidingWindow stores per-bucket counts in a ring buffer keyed by
// the floor(now/bucketSize). Sum returns the total across all live
// buckets; Rate returns sum / window-seconds.
type slidingWindow struct {
	buckets    []bucket
	bucketSize time.Duration
	window     time.Duration
}

type bucket struct {
	epoch int64 // floor(unix-nano / bucketSize.Nanoseconds())
	count int64
}

func newSlidingWindow(window, bucketSize time.Duration) *slidingWindow {
	if bucketSize <= 0 {
		bucketSize = time.Minute
	}
	if window < bucketSize {
		window = bucketSize
	}
	n := int(window / bucketSize)
	if n < 1 {
		n = 1
	}
	return &slidingWindow{
		buckets:    make([]bucket, n),
		bucketSize: bucketSize,
		window:     window,
	}
}

func (w *slidingWindow) add(now time.Time, n int64) {
	epoch := now.UnixNano() / int64(w.bucketSize)
	idx := int(epoch % int64(len(w.buckets)))
	if w.buckets[idx].epoch != epoch {
		w.buckets[idx] = bucket{epoch: epoch}
	}
	w.buckets[idx].count += n
}

func (w *slidingWindow) sum(now time.Time) int64 {
	currentEpoch := now.UnixNano() / int64(w.bucketSize)
	minEpoch := currentEpoch - int64(len(w.buckets)) + 1
	var total int64
	for _, b := range w.buckets {
		if b.epoch >= minEpoch && b.epoch <= currentEpoch {
			total += b.count
		}
	}
	return total
}

func (w *slidingWindow) ratePerSec(now time.Time) float64 {
	return float64(w.sum(now)) / w.window.Seconds()
}

// ─── HighErrorRateRule ────────────────────────────────────────────────────

// HighErrorRateRule fires when the per-service ERROR rate exceeds
// threshold (errors per second) over a sliding window. Threshold is
// in errors/sec to make rule semantics obvious; convert from "5% of
// 100 req/s" at config time, not in code.
type HighErrorRateRule struct {
	tenantID  string
	window    time.Duration
	threshold float64

	mu       sync.Mutex
	totals   map[string]*slidingWindow
	errors   map[string]*slidingWindow
	firing   map[string]bool
}

func NewHighErrorRateRule(tenantID string, window time.Duration, thresholdEPS float64) *HighErrorRateRule {
	return &HighErrorRateRule{
		tenantID:  tenantID,
		window:    window,
		threshold: thresholdEPS,
		totals:    map[string]*slidingWindow{},
		errors:    map[string]*slidingWindow{},
		firing:    map[string]bool{},
	}
}

func (r *HighErrorRateRule) Evaluate(sig signal.Signal) *Event {
	if sig.Type != signal.Log && sig.Type != signal.Trace {
		return nil
	}
	service := serviceName(sig)
	isError := sig.Attributes["severity"] == "ERROR"

	r.mu.Lock()
	defer r.mu.Unlock()

	tw, ok := r.totals[service]
	if !ok {
		tw = newSlidingWindow(r.window, time.Minute)
		r.totals[service] = tw
	}
	ew, ok := r.errors[service]
	if !ok {
		ew = newSlidingWindow(r.window, time.Minute)
		r.errors[service] = ew
	}

	now := time.Now()
	tw.add(now, 1)
	if isError {
		ew.add(now, 1)
	}

	errRate := ew.ratePerSec(now)
	totalRate := tw.ratePerSec(now)
	errorRatio := 0.0
	if totalRate > 0 {
		errorRatio = errRate / totalRate
	}

	if errRate >= r.threshold {
		if !r.firing[service] {
			r.firing[service] = true
			return &Event{
				Type:      HighErrorRate,
				TenantID:  r.tenantID,
				Timestamp: now,
				Data: map[string]any{
					"service":    service,
					"error_rate": errRate,
					"error_ratio": errorRatio,
					"total_rate": totalRate,
					"window":     r.window.String(),
				},
			}
		}
		return nil
	}
	// Rate has dropped — reset edge so the next breach re-fires.
	r.firing[service] = false
	return nil
}

// ─── HighLatencyRule ──────────────────────────────────────────────────────

// HighLatencyRule fires when the per-service p95-ish latency in the
// window exceeds threshold milliseconds. We approximate p95 with the
// max observed in the window — cheap and good-enough for an alerting
// signal. Real percentile estimators (t-digest, HDR) live in Phase C.
type HighLatencyRule struct {
	tenantID  string
	window    time.Duration
	threshold float64 // ms

	mu     sync.Mutex
	maxes  map[string]*latencyTracker
	firing map[string]bool
}

type latencyTracker struct {
	buckets    []latencyBucket
	bucketSize time.Duration
	window     time.Duration
}

type latencyBucket struct {
	epoch int64
	maxMS float64
}

func newLatencyTracker(window, bucketSize time.Duration) *latencyTracker {
	if bucketSize <= 0 {
		bucketSize = time.Minute
	}
	if window < bucketSize {
		window = bucketSize
	}
	n := int(window / bucketSize)
	if n < 1 {
		n = 1
	}
	return &latencyTracker{
		buckets:    make([]latencyBucket, n),
		bucketSize: bucketSize,
		window:     window,
	}
}

func (l *latencyTracker) observe(now time.Time, ms float64) {
	epoch := now.UnixNano() / int64(l.bucketSize)
	idx := int(epoch % int64(len(l.buckets)))
	if l.buckets[idx].epoch != epoch {
		l.buckets[idx] = latencyBucket{epoch: epoch, maxMS: ms}
	} else if ms > l.buckets[idx].maxMS {
		l.buckets[idx].maxMS = ms
	}
}

func (l *latencyTracker) maxIn(now time.Time) float64 {
	currentEpoch := now.UnixNano() / int64(l.bucketSize)
	minEpoch := currentEpoch - int64(len(l.buckets)) + 1
	var m float64
	for _, b := range l.buckets {
		if b.epoch >= minEpoch && b.epoch <= currentEpoch && b.maxMS > m {
			m = b.maxMS
		}
	}
	return m
}

func NewHighLatencyRule(tenantID string, window time.Duration, thresholdMS float64) *HighLatencyRule {
	return &HighLatencyRule{
		tenantID:  tenantID,
		window:    window,
		threshold: thresholdMS,
		maxes:     map[string]*latencyTracker{},
		firing:    map[string]bool{},
	}
}

func (r *HighLatencyRule) Evaluate(sig signal.Signal) *Event {
	if sig.Type != signal.Trace {
		return nil
	}
	durStr, ok := sig.Attributes["duration_ms"]
	if !ok {
		return nil
	}
	dur, err := strconv.ParseFloat(durStr, 64)
	if err != nil {
		return nil
	}
	service := serviceName(sig)

	r.mu.Lock()
	defer r.mu.Unlock()

	lt, ok := r.maxes[service]
	if !ok {
		lt = newLatencyTracker(r.window, time.Minute)
		r.maxes[service] = lt
	}
	now := time.Now()
	lt.observe(now, dur)

	peak := lt.maxIn(now)
	if peak >= r.threshold {
		if !r.firing[service] {
			r.firing[service] = true
			return &Event{
				Type:      HighLatency,
				TenantID:  r.tenantID,
				Timestamp: now,
				Data: map[string]any{
					"service":      service,
					"peak_latency": peak,
					"threshold":    r.threshold,
					"window":       r.window.String(),
				},
			}
		}
		return nil
	}
	r.firing[service] = false
	return nil
}

// ─── Engine ───────────────────────────────────────────────────────────────

type Engine struct {
	mu    sync.RWMutex
	rules []Rule
}

func NewEngine() *Engine {
	return &Engine{rules: []Rule{}}
}

func (e *Engine) AddRule(r Rule) {
	e.mu.Lock()
	e.rules = append(e.rules, r)
	e.mu.Unlock()
}

func (e *Engine) Process(_ context.Context, sig signal.Signal) *Event {
	e.mu.RLock()
	rules := e.rules
	e.mu.RUnlock()
	for _, r := range rules {
		if ev := r.Evaluate(sig); ev != nil {
			return ev
		}
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────

func serviceName(sig signal.Signal) string {
	if s, ok := sig.Attributes["service.name"]; ok && s != "" {
		return s
	}
	if s, ok := sig.Attributes["service_name"]; ok && s != "" {
		return s
	}
	return "unknown"
}
