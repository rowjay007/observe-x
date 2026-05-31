// Package sampling decides which traces to keep for downstream
// retention. Phase B-4 adds an EWMA-baseline z-score component to the
// per-trace score so traces whose latency is anomalously high relative
// to *that service's* recent baseline get prioritised, not just
// traces over an arbitrary global threshold.
//
//	score = base                       (Phase A: severity + duration + service)
//	      + 10 × latency_zscore        (Phase B-4: anomaly relative to baseline)
//	      + 25 if parent_sampled       (Phase B-4: propagate upstream decision)
//
// Optionally, sampler state can be persisted to a StateStore (Redis
// in production) every FlushInterval so a process restart doesn't
// wipe the learned baselines. The hot path is always in-memory; the
// store is a best-effort write-behind.
package sampling

import (
	"container/heap"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
)

type SampleDecision int

const (
	Keep SampleDecision = iota
	Drop
)

type TraceScore struct {
	TraceID   string
	Score     float64
	Timestamp time.Time
	Index     int
}

type PriorityQueue []*TraceScore

func (pq PriorityQueue) Len() int { return len(pq) }
func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].Index = i
	pq[j].Index = j
}

func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*TraceScore)
	item.Index = n
	*pq = append(*pq, item)
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.Index = -1
	*pq = old[0 : n-1]
	return item
}

type AdaptiveSampler struct {
	mu           sync.Mutex
	pq           PriorityQueue
	seenTraces   map[string]time.Time
	traceIndex   map[string]*TraceScore
	samplingRate float64
	maxSize      int

	// Phase B-4 additions.
	latency *ewmaTracker
	state   StateStore
	tenant  string

	flushInterval time.Duration
	stopCh        chan struct{}
	doneCh        chan struct{}
}

func NewAdaptiveSampler(samplingRate float64, maxSize int) *AdaptiveSampler {
	return NewAdaptiveSamplerWithOptions(SamplerOptions{
		SamplingRate: samplingRate,
		MaxSize:      maxSize,
	})
}

// SamplerOptions configures NewAdaptiveSamplerWithOptions. Zero values
// are filled with safe defaults; only SamplingRate and MaxSize are
// strictly required.
type SamplerOptions struct {
	SamplingRate float64
	MaxSize      int
	// EWMAAlpha controls how fast the latency baseline adapts.
	// Default 0.05 (~20-sample half-life).
	EWMAAlpha float64
	// TenantID — opaque scope key for the StateStore. May be empty
	// when the sampler isn't tenant-scoped.
	TenantID string
	// State — optional persistent state store. nil → in-memory only.
	State StateStore
	// FlushInterval — how often a snapshot of EWMA state is written
	// to State. Default 30s.
	FlushInterval time.Duration
}

func NewAdaptiveSamplerWithOptions(opts SamplerOptions) *AdaptiveSampler {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 30 * time.Second
	}
	s := &AdaptiveSampler{
		pq:            make(PriorityQueue, 0),
		seenTraces:    make(map[string]time.Time),
		traceIndex:    make(map[string]*TraceScore),
		samplingRate:  opts.SamplingRate,
		maxSize:       opts.MaxSize,
		latency:       newEWMATracker(opts.EWMAAlpha),
		state:         opts.State,
		tenant:        opts.TenantID,
		flushInterval: opts.FlushInterval,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	heap.Init(&s.pq)
	if s.state != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if loaded, err := s.state.Load(ctx, s.tenant); err == nil && len(loaded) > 0 {
			s.latency.restore(loaded)
		}
		go s.flushLoop()
	} else {
		close(s.doneCh)
	}
	return s
}

// Close stops the background flusher and writes a final snapshot.
// Safe to call on a sampler without a StateStore (no-op).
func (s *AdaptiveSampler) Close() error {
	if s.state == nil {
		return nil
	}
	select {
	case <-s.stopCh:
		return nil
	default:
		close(s.stopCh)
	}
	<-s.doneCh
	return nil
}

func (s *AdaptiveSampler) flushLoop() {
	defer close(s.doneCh)
	t := time.NewTicker(s.flushInterval)
	defer t.Stop()
	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.state.Save(ctx, s.tenant, s.latency.snapshot())
	}
	for {
		select {
		case <-t.C:
			flush()
		case <-s.stopCh:
			flush()
			return
		}
	}
}

func (s PriorityQueue) Less(i, j int) bool {
	if s[i].Score == s[j].Score {
		return s[i].Timestamp.Before(s[j].Timestamp)
	}
	return s[i].Score < s[j].Score
}

func (s *AdaptiveSampler) Score(sig signal.Signal) float64 {
	if sig.Type != signal.Trace {
		return 0
	}

	score := 0.0

	if sig.Attributes["severity"] == "ERROR" {
		score += 100.0
	}

	service := sig.Attributes["service.name"]
	if service == "" {
		service = sig.Attributes["service_name"]
	}

	if duration, ok := sig.Attributes["duration_ms"]; ok {
		if d, err := strconv.ParseFloat(duration, 64); err == nil {
			if d > 1000 {
				score += 50.0
			}
			// Anomaly relative to this service's baseline. The Score
			// method intentionally also feeds the tracker so the model
			// adapts over time; this is correct because every signal
			// we see is observed exactly once.
			if service != "" {
				z := s.latency.zscore(service, d)
				if z > 0 {
					score += 10.0 * z
				}
				s.latency.observe(service, d)
			}
		}
	}

	if sig.Attributes["service.name"] == "payment-service" ||
		sig.Attributes["service_name"] == "payment-service" {
		score += 20.0
	}

	if sig.Attributes["parent_sampled"] == "true" {
		score += 25.0
	}

	return score
}

func (s *AdaptiveSampler) Decide(sig signal.Signal) SampleDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sig.Type != signal.Trace {
		return Keep
	}

	score := s.Score(sig)
	traceID, _ := sig.Attributes["trace_id"]
	if traceID == "" {
		return Keep
	}

	lastSeen, seenBefore := s.seenTraces[traceID]
	currentTime := time.Now()

	item, exists := s.traceIndex[traceID]
	if exists {
		item.Score = score
		item.Timestamp = currentTime
		heap.Fix(&s.pq, item.Index)
	} else {
		item = &TraceScore{
			TraceID:   traceID,
			Score:     score,
			Timestamp: currentTime,
		}
		heap.Push(&s.pq, item)
		s.traceIndex[traceID] = item
	}

	s.seenTraces[traceID] = currentTime

	for s.pq.Len() > s.maxSize {
		evicted := heap.Pop(&s.pq).(*TraceScore)
		delete(s.traceIndex, evicted.TraceID)
	}

	if score >= 50.0 {
		return Keep
	}

	if seenBefore && time.Since(lastSeen) < 5*time.Minute {
		return Keep
	}

	keepThreshold := int(float64(s.maxSize) * s.samplingRate)
	if keepThreshold < 1 {
		keepThreshold = 1
	}

	if s.pq.Len() <= keepThreshold {
		return Keep
	}

	return Drop
}
