package supervisor

import (
	"context"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/actor"
	"github.com/rowjay007/observe-x/pkg/signal"
)

type Stats struct {
	ActorCount   int
	TotalRouted  int64
	LastHealthAt time.Time
}

type Supervisor struct {
	actors       map[string]*actor.TenantActor
	mu           sync.RWMutex
	ctx          context.Context
	cancel       context.CancelFunc
	started      bool
	stopOnce     sync.Once
	totalRouted  int64
	lastHealthAt time.Time
}

func NewSupervisor() *Supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		actors: make(map[string]*actor.TenantActor),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *Supervisor) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	go s.monitor()
}

func (s *Supervisor) Stop() error {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, tenantActor := range s.actors {
			_ = tenantActor.Stop()
		}
		s.actors = make(map[string]*actor.TenantActor)
	})
	return nil
}

func (s *Supervisor) GetOrCreateActor(tenantID string) *actor.TenantActor {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.actors[tenantID]; ok {
		return existing
	}

	newActor := actor.NewTenantActor(tenantID, 4096)
	s.actors[tenantID] = newActor
	_ = newActor.Start(s.ctx)
	return newActor
}

func (s *Supervisor) RouteToTenant(tenantID string, sig signal.Signal) {
	actor := s.GetOrCreateActor(tenantID)
	if actor == nil {
		return
	}

	select {
	case actor.Mailbox() <- sig:
		s.mu.Lock()
		s.totalRouted++
		s.mu.Unlock()
	default:
	}
}

func (s *Supervisor) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return Stats{
		ActorCount:   len(s.actors),
		TotalRouted:  s.totalRouted,
		LastHealthAt: s.lastHealthAt,
	}
}

func (s *Supervisor) monitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.checkHealth()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Supervisor) checkHealth() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastHealthAt = time.Now()
	for tenantID, tenantActor := range s.actors {
		if !tenantActor.IsRunning() {
			delete(s.actors, tenantID)
		}
	}
}
