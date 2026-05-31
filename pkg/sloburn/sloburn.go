// Package sloburn implements the multi-window multi-burn-rate alert
// algorithm from the Google SRE Workbook (chapter 5, "Alerting on
// SLOs"). Given an SLO target (error budget) and a rolling sample of
// good/bad event counts, the evaluator decides whether an alert
// should fire — and at what severity (page vs ticket) — based on the
// rate at which the budget is being consumed across multiple time
// windows simultaneously.
//
// The standard pairs (used by Google's own production teams) are:
//
//   page:    14.4× burn in the last 1h   AND  14.4× burn in the last 5m
//   page:    6×    burn in the last 6h   AND  6×    burn in the last 30m
//   ticket:  3×    burn in the last 24h  AND  3×    burn in the last 2h
//   ticket:  1×    burn in the last 3d   AND  1×    burn in the last 6h
//
// The "long" window catches sustained burn; the "short" window
// catches the early phase of the same burn and ensures the alert
// fires quickly. Both must trip simultaneously, which keeps the
// false-positive rate manageable for the page-level severity.
//
// See ADR-0009 (alert-manager) for how this fits the wider service.
package sloburn

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Severity string

const (
	SevPage   Severity = "page"
	SevTicket Severity = "ticket"
	SevOK     Severity = "ok"
)

// Window pairs (long, short, burn-multiplier, severity). Order is
// significant: evaluators check from most severe to least so the
// first match wins.
type WindowPair struct {
	Long, Short  time.Duration
	BurnRate     float64
	Severity     Severity
}

// DefaultWindowPairs returns the SRE Workbook standard pairs.
func DefaultWindowPairs() []WindowPair {
	return []WindowPair{
		{Long: 1 * time.Hour, Short: 5 * time.Minute, BurnRate: 14.4, Severity: SevPage},
		{Long: 6 * time.Hour, Short: 30 * time.Minute, BurnRate: 6.0, Severity: SevPage},
		{Long: 24 * time.Hour, Short: 2 * time.Hour, BurnRate: 3.0, Severity: SevTicket},
		{Long: 72 * time.Hour, Short: 6 * time.Hour, BurnRate: 1.0, Severity: SevTicket},
	}
}

// ─── SLO definition ───────────────────────────────────────────────────────

// SLO carries the parameters that anchor the burn calculation.
type SLO struct {
	Name        string
	Target      float64 // e.g. 0.999 for "99.9% good events"
	WindowPairs []WindowPair
}

func (s SLO) errorBudget() float64 { return 1 - s.Target }

func (s SLO) validate() error {
	if s.Name == "" {
		return errors.New("sloburn: SLO.Name required")
	}
	if s.Target <= 0 || s.Target >= 1 {
		return fmt.Errorf("sloburn: SLO.Target must be in (0,1), got %v", s.Target)
	}
	if len(s.WindowPairs) == 0 {
		return errors.New("sloburn: SLO.WindowPairs required")
	}
	return nil
}

// ─── Event store: bucketed good/bad counters ──────────────────────────────

// Evaluator holds the rolling event counters per SLO. It is safe for
// concurrent use. Buckets are 1 minute wide; the longest configured
// window determines retention.
type Evaluator struct {
	mu       sync.RWMutex
	slos     map[string]*sloState
	bucket   time.Duration
}

type sloState struct {
	slo     SLO
	maxKeep time.Duration
	good    []bucket
	bad     []bucket
}

type bucket struct {
	epoch    int64 // minute-floored unix epoch
	good     int64
	bad      int64
}

func New() *Evaluator {
	return &Evaluator{
		slos:   map[string]*sloState{},
		bucket: time.Minute,
	}
}

// Register validates and stores an SLO definition. Re-registering an
// SLO under the same Name replaces the previous definition; existing
// counters are preserved.
func (e *Evaluator) Register(slo SLO) error {
	if err := slo.validate(); err != nil {
		return err
	}
	var maxKeep time.Duration
	for _, p := range slo.WindowPairs {
		if p.Long > maxKeep {
			maxKeep = p.Long
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.slos[slo.Name]; ok {
		s.slo = slo
		s.maxKeep = maxKeep
		return nil
	}
	e.slos[slo.Name] = &sloState{slo: slo, maxKeep: maxKeep}
	return nil
}

// Observe records one event against an SLO. good=true increments the
// good counter; good=false increments the bad counter. The choice of
// what counts as good vs bad is the caller's responsibility (e.g.
// HTTP 5xx = bad, anything else = good, OR latency-bucket-based).
func (e *Evaluator) Observe(sloName string, good bool, at time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.slos[sloName]
	if !ok {
		return fmt.Errorf("sloburn: unknown SLO %q", sloName)
	}
	epoch := at.UTC().Unix() / int64(e.bucket.Seconds())

	if good {
		s.good = addToBucket(s.good, epoch, 1, 0)
	} else {
		s.bad = addToBucket(s.bad, epoch, 0, 1)
	}
	cutoff := at.Add(-s.maxKeep - time.Minute).UTC().Unix() / int64(e.bucket.Seconds())
	s.good = trimOlder(s.good, cutoff)
	s.bad = trimOlder(s.bad, cutoff)
	return nil
}

// ─── Evaluation ──────────────────────────────────────────────────────────

// Decision is the result of Evaluate. Severity is SevOK if no pair
// trips, otherwise the highest-severity matched pair is returned.
type Decision struct {
	SLO           string
	Severity      Severity
	BurnLong      float64
	BurnShort     float64
	Pair          WindowPair
	ErrorBudget   float64
	EvaluatedAt   time.Time
}

// Evaluate walks the SLO's window pairs from highest to lowest
// severity. The first pair whose long-window AND short-window burn
// rates both exceed the configured BurnRate fires.
func (e *Evaluator) Evaluate(sloName string, now time.Time) (Decision, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.slos[sloName]
	if !ok {
		return Decision{}, fmt.Errorf("sloburn: unknown SLO %q", sloName)
	}
	d := Decision{SLO: sloName, Severity: SevOK, ErrorBudget: s.slo.errorBudget(), EvaluatedAt: now}

	pairs := append([]WindowPair{}, s.slo.WindowPairs...)
	sort.SliceStable(pairs, func(i, j int) bool {
		return severityRank(pairs[i].Severity) > severityRank(pairs[j].Severity)
	})

	for _, p := range pairs {
		long := burnRate(s, p.Long, now, s.slo.errorBudget())
		short := burnRate(s, p.Short, now, s.slo.errorBudget())
		if long >= p.BurnRate && short >= p.BurnRate {
			d.Severity = p.Severity
			d.BurnLong = long
			d.BurnShort = short
			d.Pair = p
			return d, nil
		}
		// Record the loudest near-miss too — useful for debugging.
		if d.BurnLong < long {
			d.BurnLong = long
			d.BurnShort = short
			d.Pair = p
		}
	}
	return d, nil
}

// burnRate returns the observed error rate over window, expressed
// as a multiplier of the SLO's error budget. A burn rate of 1.0
// means "exactly consuming the budget at the target pace"; 14.4
// means "consuming the entire budget in window/14.4 wall time".
func burnRate(s *sloState, window time.Duration, now time.Time, budget float64) float64 {
	if budget <= 0 {
		return 0
	}
	cutoff := now.Add(-window).UTC().Unix() / 60
	end := now.UTC().Unix() / 60

	var good, bad int64
	for _, b := range s.good {
		if b.epoch > cutoff && b.epoch <= end {
			good += b.good
		}
	}
	for _, b := range s.bad {
		if b.epoch > cutoff && b.epoch <= end {
			bad += b.bad
		}
	}
	total := good + bad
	if total == 0 {
		return 0
	}
	errorRate := float64(bad) / float64(total)
	return errorRate / budget
}

func severityRank(s Severity) int {
	switch s {
	case SevPage:
		return 2
	case SevTicket:
		return 1
	}
	return 0
}

// ─── bucket helpers ───────────────────────────────────────────────────────

func addToBucket(bs []bucket, epoch int64, g, b int64) []bucket {
	for i := range bs {
		if bs[i].epoch == epoch {
			bs[i].good += g
			bs[i].bad += b
			return bs
		}
	}
	return append(bs, bucket{epoch: epoch, good: g, bad: b})
}

func trimOlder(bs []bucket, cutoff int64) []bucket {
	out := bs[:0]
	for _, b := range bs {
		if b.epoch > cutoff {
			out = append(out, b)
		}
	}
	return out
}
