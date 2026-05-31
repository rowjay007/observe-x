// Package supervisor manages the per-tenant TenantActor population.
//
// Phase B-4 replaces the Phase A polling supervisor with a proper
// one-for-one restart strategy modelled on Erlang/OTP:
//
//   * Each actor is launched in its own goroutine wrapped in a
//     panic recovery + run-loop wrapper.
//   * On crash, the supervisor restarts the actor with an
//     exponential backoff (250ms → 30s, capped).
//   * Restarts are tracked per actor in a sliding window; if more
//     than MaxRestarts happen within RestartWindow, the actor is
//     promoted to PERMANENT (will not be auto-recreated until a
//     human revokes the quarantine) and all subsequent mail to that
//     tenant is dropped with a "tenant-quarantined" metric.
//
// The supervisor is intentionally not OTP-faithful: we have no need
// for rest_for_one or one_for_all because tenant actors do not share
// state. The OTP terminology is borrowed because it's the lingua
// franca for this pattern.
//
// See docs/adr/0006-stream-processor-v2.md.
package supervisor

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rowjay007/observe-x/pkg/actor"
	"github.com/rowjay007/observe-x/pkg/signal"
)

// ─── public types ─────────────────────────────────────────────────────────

type Stats struct {
	ActorCount    int
	Quarantined   int
	TotalRouted   int64
	TotalDropped  int64
	TotalRestarts int64
	LastHealthAt  time.Time
}

type Options struct {
	// MailboxSize is the per-tenant inbox capacity. Default 4096.
	MailboxSize int
	// MaxRestarts allowed per RestartWindow before quarantine.
	// Default: 5 restarts per minute.
	MaxRestarts   int
	RestartWindow time.Duration
	// BackoffMin / BackoffMax bound the exponential restart delay.
	BackoffMin time.Duration
	BackoffMax time.Duration
	// HealthInterval — how often the monitor loop sweeps actors.
	HealthInterval time.Duration
}

func (o Options) withDefaults() Options {
	if o.MailboxSize <= 0 {
		o.MailboxSize = 4096
	}
	if o.MaxRestarts <= 0 {
		o.MaxRestarts = 5
	}
	if o.RestartWindow <= 0 {
		o.RestartWindow = time.Minute
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = 250 * time.Millisecond
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 30 * time.Second
	}
	if o.HealthInterval <= 0 {
		o.HealthInterval = 5 * time.Second
	}
	return o
}

// ─── managedActor — the supervisor's bookkeeping per actor ────────────────

type managedActor struct {
	tenantID    string
	a           *actor.TenantActor
	restarts    []time.Time
	backoffNext time.Duration
	quarantined bool
}

// ─── Supervisor ───────────────────────────────────────────────────────────

type Supervisor struct {
	opts Options

	mu      sync.RWMutex
	actors  map[string]*managedActor
	rootCtx context.Context
	cancel  context.CancelFunc

	totalRouted   atomic.Int64
	totalDropped  atomic.Int64
	totalRestarts atomic.Int64

	started  atomic.Bool
	stopOnce sync.Once
	lastHealthAt atomic.Int64 // unix-nano

	// hooks
	onCrash func(tenantID string, err any) // optional, test hook
}

func NewSupervisor() *Supervisor {
	return NewSupervisorWithOptions(Options{})
}

func NewSupervisorWithOptions(opts Options) *Supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		opts:    opts.withDefaults(),
		actors:  make(map[string]*managedActor),
		rootCtx: ctx,
		cancel:  cancel,
	}
}

// Start launches the monitor goroutine. Idempotent.
func (s *Supervisor) Start() {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	go s.monitor()
}

func (s *Supervisor) Stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		for _, m := range s.actors {
			_ = m.a.Stop()
		}
		s.actors = map[string]*managedActor{}
		s.mu.Unlock()
	})
	return nil
}

// ─── routing ──────────────────────────────────────────────────────────────

// RouteToTenant gets-or-creates the tenant actor and tries to deliver
// the signal. Returns immediately; if the inbox is full the signal is
// dropped and counted.
func (s *Supervisor) RouteToTenant(tenantID string, sig signal.Signal) {
	m := s.getOrCreate(tenantID)
	if m == nil { // quarantined
		s.totalDropped.Add(1)
		return
	}
	select {
	case m.a.Mailbox() <- sig:
		s.totalRouted.Add(1)
	default:
		s.totalDropped.Add(1)
	}
}

func (s *Supervisor) getOrCreate(tenantID string) *managedActor {
	s.mu.RLock()
	if m, ok := s.actors[tenantID]; ok && !m.quarantined {
		s.mu.RUnlock()
		return m
	} else if ok && m.quarantined {
		s.mu.RUnlock()
		return nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.actors[tenantID]; ok {
		if m.quarantined {
			return nil
		}
		return m
	}
	a := actor.NewTenantActor(tenantID, s.opts.MailboxSize)
	if err := a.Start(s.rootCtx); err != nil {
		return nil
	}
	m := &managedActor{
		tenantID:    tenantID,
		a:           a,
		backoffNext: s.opts.BackoffMin,
	}
	s.actors[tenantID] = m
	return m
}

// ─── monitor loop ─────────────────────────────────────────────────────────

func (s *Supervisor) monitor() {
	t := time.NewTicker(s.opts.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.rootCtx.Done():
			return
		}
	}
}

func (s *Supervisor) sweep() {
	s.lastHealthAt.Store(time.Now().UnixNano())
	now := time.Now()
	cutoff := now.Add(-s.opts.RestartWindow)

	s.mu.Lock()
	defer s.mu.Unlock()

	for tenantID, m := range s.actors {
		if m.quarantined {
			continue
		}
		if m.a.IsRunning() {
			// Trim the restart history to keep the window honest.
			m.restarts = trimBefore(m.restarts, cutoff)
			continue
		}
		// Actor stopped — supervise it.
		s.totalRestarts.Add(1)
		m.restarts = append(trimBefore(m.restarts, cutoff), now)
		if len(m.restarts) > s.opts.MaxRestarts {
			m.quarantined = true
			if s.onCrash != nil {
				s.onCrash(tenantID, fmt.Sprintf(
					"quarantined after %d restarts in %s",
					len(m.restarts), s.opts.RestartWindow))
			}
			continue
		}

		delay := m.backoffNext
		m.backoffNext = nextBackoff(m.backoffNext, s.opts.BackoffMax)

		// Spawn a goroutine to wait out the backoff and restart;
		// don't block the sweep on it.
		mCopy := m
		go func() {
			select {
			case <-time.After(delay):
			case <-s.rootCtx.Done():
				return
			}
			s.restartActor(mCopy)
		}()
	}
}

func (s *Supervisor) restartActor(m *managedActor) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-check under lock; situation may have changed.
	if m.quarantined {
		return
	}
	if m.a.IsRunning() {
		return
	}

	fresh := actor.NewTenantActor(m.tenantID, s.opts.MailboxSize)
	if err := fresh.Start(s.rootCtx); err != nil {
		return
	}
	m.a = fresh
}

func trimBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for ; i < len(ts); i++ {
		if !ts[i].Before(cutoff) {
			break
		}
	}
	return ts[i:]
}

func nextBackoff(curr, max time.Duration) time.Duration {
	next := curr * 2
	// Add jitter ±20% to avoid thundering-herd restarts.
	jitter := time.Duration(float64(next) * (rand.Float64()*0.4 - 0.2))
	next += jitter
	if next > max {
		next = max
	}
	if next < 0 {
		next = curr
	}
	return next
}

// ─── observability ────────────────────────────────────────────────────────

func (s *Supervisor) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	quarantined := 0
	for _, m := range s.actors {
		if m.quarantined {
			quarantined++
		}
	}
	return Stats{
		ActorCount:    len(s.actors),
		Quarantined:   quarantined,
		TotalRouted:   s.totalRouted.Load(),
		TotalDropped:  s.totalDropped.Load(),
		TotalRestarts: s.totalRestarts.Load(),
		LastHealthAt:  time.Unix(0, s.lastHealthAt.Load()),
	}
}

// GetOrCreateActor is retained for Phase A callers that need the actor
// handle directly (e.g. tests). Production routing should use
// RouteToTenant instead.
func (s *Supervisor) GetOrCreateActor(tenantID string) *actor.TenantActor {
	m := s.getOrCreate(tenantID)
	if m == nil {
		return nil
	}
	return m.a
}

// ReleaseQuarantine clears the quarantine flag for a tenant so the
// next routed signal recreates the actor. Intended for operator use.
func (s *Supervisor) ReleaseQuarantine(tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.actors[tenantID]; ok {
		m.quarantined = false
		m.restarts = nil
		m.backoffNext = s.opts.BackoffMin
		delete(s.actors, tenantID) // force fresh on next route
	}
}
