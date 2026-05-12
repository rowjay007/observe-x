package cep

import (
	"context"
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
	Data      map[string]interface{}
}

type Rule interface {
	Evaluate(sig signal.Signal) *Event
}

type HighErrorRateRule struct {
	tenantID       string
	windowDuration time.Duration
	errorThreshold float64
	counters       map[string]int
	mu             sync.Mutex
}

func NewHighErrorRateRule(tenantID string, window time.Duration, threshold float64) *HighErrorRateRule {
	return &HighErrorRateRule{
		tenantID:       tenantID,
		windowDuration: window,
		errorThreshold: threshold,
		counters:       make(map[string]int),
	}
}

func (r *HighErrorRateRule) Evaluate(sig signal.Signal) *Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sig.Type != signal.Log {
		return nil
	}

	severity, ok := sig.Attributes["severity"]
	if !ok || severity != "ERROR" {
		return nil
	}

	service := sig.Attributes["service_name"]
	if service == "" {
		service = "unknown"
	}

	r.counters[service]++

	totalRequests := r.counters[service]
	errorRate := float64(totalRequests) / 100.0

	if errorRate > r.errorThreshold {
		return &Event{
			Type:      HighErrorRate,
			TenantID:  r.tenantID,
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"service":    service,
				"error_rate": errorRate,
			},
		}
	}

	return nil
}

type Engine struct {
	rules []Rule
}

func NewEngine() *Engine {
	return &Engine{rules: make([]Rule, 0)}
}

func (e *Engine) AddRule(rule Rule) {
	e.rules = append(e.rules, rule)
}

func (e *Engine) Process(ctx context.Context, sig signal.Signal) *Event {
	for _, rule := range e.rules {
		if event := rule.Evaluate(sig); event != nil {
			return event
		}
	}
	return nil
}
