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

func NewTenantActor(tenantID string, bufferSize int) *TenantActor {
	return &TenantActor{
		tenantID:  tenantID,
		inbox:     make(chan signal.Signal, bufferSize),
		cepEngine: cep.NewEngine(),
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

	a.cepEngine.AddRule(cep.NewHighErrorRateRule(a.tenantID, 5*time.Minute, 0.05))

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
