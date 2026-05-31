// Package actor implements the per-tenant TenantActor.
//
// Phase A scope: each TenantActor runs the per-tenant CEP rules and
// exposes Stats for observability. Sampling lives one level up in the
// engine (see ADR-0001). The actor is intentionally cheap so the
// supervisor can keep tens of thousands alive concurrently.
package actor

import (
	"context"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/cep"
	"github.com/rowjay007/observe-x/pkg/signal"
)

type Actor interface {
	Start(ctx context.Context) error
	Stop() error
	Mailbox() chan<- signal.Signal
}

type Stats struct {
	Processed      int64
	Dropped        int64
	LastSignalType signal.Type
	LastEventType  cep.EventType
	LastSignalAt   time.Time
	LastEventAt    time.Time
	Running        bool
}

type TenantActor struct {
	tenantID  string
	inbox     chan signal.Signal
	cepEngine *cep.Engine
	opts      Options

	mu             sync.RWMutex
	stopOnce       sync.Once
	running        bool
	processed      int64
	dropped        int64
	lastSignalType signal.Type
	lastEventType  cep.EventType
	lastSignalAt   time.Time
	lastEventAt    time.Time
}

// Options tunes the CEP rule thresholds that the actor registers on
// Start. Zero values fall back to production-safe defaults (see
// withDefaults). Tests can supply tighter values for tiny synthetic
// bursts; Phase C will surface per-tenant overrides via tenant-api.
type Options struct {
	ErrorRateThresholdEPS float64       // errors per second
	LatencyThresholdMS    float64       // peak ms
	Window                time.Duration // CEP sliding window
}

func (o Options) withDefaults() Options {
	if o.ErrorRateThresholdEPS <= 0 {
		o.ErrorRateThresholdEPS = 1.0
	}
	if o.LatencyThresholdMS <= 0 {
		o.LatencyThresholdMS = 1000.0
	}
	if o.Window <= 0 {
		o.Window = 5 * time.Minute
	}
	return o
}

func NewTenantActor(tenantID string, bufferSize int) *TenantActor {
	return NewTenantActorWithOptions(tenantID, bufferSize, Options{})
}

func NewTenantActorWithOptions(tenantID string, bufferSize int, opts Options) *TenantActor {
	return &TenantActor{
		tenantID:  tenantID,
		inbox:     make(chan signal.Signal, bufferSize),
		cepEngine: cep.NewEngine(),
		opts:      opts.withDefaults(),
	}
}

func (a *TenantActor) Mailbox() chan<- signal.Signal {
	return a.inbox
}

func (a *TenantActor) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = true
	a.mu.Unlock()

	// Phase B-4: thresholds are now errors/sec and ms (see ADR-0006).
	// Per-tenant overrides land in Phase C; for now a global default
	// or per-actor Options drive both rules.
	a.cepEngine.AddRule(cep.NewHighErrorRateRule(a.tenantID, a.opts.Window, a.opts.ErrorRateThresholdEPS))
	a.cepEngine.AddRule(cep.NewHighLatencyRule(a.tenantID, a.opts.Window, a.opts.LatencyThresholdMS))

	go func() {
		for {
			select {
			case sig, ok := <-a.inbox:
				if !ok {
					return
				}
				a.processSignal(sig)
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (a *TenantActor) Stop() error {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
		close(a.inbox)
	})
	return nil
}

func (a *TenantActor) IsRunning() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.running
}

func (a *TenantActor) Stats() Stats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return Stats{
		Processed:      a.processed,
		Dropped:        a.dropped,
		LastSignalType: a.lastSignalType,
		LastEventType:  a.lastEventType,
		LastSignalAt:   a.lastSignalAt,
		LastEventAt:    a.lastEventAt,
		Running:        a.running,
	}
}

func (a *TenantActor) processSignal(sig signal.Signal) {
	now := time.Now()

	a.mu.Lock()
	a.processed++
	a.lastSignalType = sig.Type
	a.lastSignalAt = now
	a.mu.Unlock()

	event := a.cepEngine.Process(context.Background(), sig)
	if event != nil {
		a.mu.Lock()
		a.lastEventType = event.Type
		a.lastEventAt = event.Timestamp
		a.mu.Unlock()
	}
}
