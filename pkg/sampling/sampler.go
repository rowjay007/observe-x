package sampling

import (
	"container/heap"
	"github.com/rowjay007/observe-x/pkg/signal"
	"strconv"
	"sync"
	"time"
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
}

func NewAdaptiveSampler(samplingRate float64, maxSize int) *AdaptiveSampler {
	sampler := &AdaptiveSampler{
		pq:           make(PriorityQueue, 0),
		seenTraces:   make(map[string]time.Time),
		traceIndex:   make(map[string]*TraceScore),
		samplingRate: samplingRate,
		maxSize:      maxSize,
	}
	heap.Init(&sampler.pq)
	return sampler
}

func (s PriorityQueue) Less(i, j int) bool {
	if s[i].Score == s[j].Score {
		return s[i].Timestamp.Before(s[j].Timestamp)
	}
	return s[i].Score < s[j].Score
}

func (s *AdaptiveSampler) Score(sig signal.Signal) float64 {
	score := 0.0

	if sig.Type == signal.Trace {
		if severity, ok := sig.Attributes["severity"]; ok && severity == "ERROR" {
			score += 100.0
		}

		if duration, ok := sig.Attributes["duration_ms"]; ok {
			if d, err := strconv.ParseFloat(duration, 64); err == nil && d > 1000 {
				score += 50.0
			}
		}

		if sig.Attributes["service_name"] == "payment-service" {
			score += 20.0
		}
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
