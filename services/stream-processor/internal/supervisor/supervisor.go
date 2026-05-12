package supervisor

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/services/stream-processor/internal/actor"
)

type Supervisor struct {
	actors map[string]*actor.TenantActor
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
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
	go s.monitor()
}

func (s *Supervisor) Stop() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.actors {
		a.Stop()
	}
}

func (s *Supervisor) GetOrCreateActor(tenantID string) *actor.TenantActor {
	s.mu.Lock()
	defer s.mu.Unlock()

	if a, exists := s.actors[tenantID]; exists {
		return a
	}

	newActor := actor.NewTenantActor(tenantID, 4096)
	s.actors[tenantID] = newActor
	newActor.Start(s.ctx)
	return newActor
}

func (s *Supervisor) RouteToTenant(tenantID string, sig signal.Signal) {
	actor := s.GetOrCreateActor(tenantID)
	select {
	case actor.Mailbox() <- sig:
	default:
		log.Printf("Actor mailbox full for tenant %s, dropping signal", tenantID)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.actors {
		_ = a
	}
}
