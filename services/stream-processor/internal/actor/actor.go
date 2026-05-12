package actor

import (
	"context"

	"github.com/rowjay007/observe-x/pkg/signal"
)

type Actor interface {
	Start(ctx context.Context) error
	Stop() error
	Mailbox() chan<- signal.Signal
}

type TenantActor struct {
	tenantID string
	inbox    chan signal.Signal
}

func NewTenantActor(tenantID string, bufferSize int) *TenantActor {
	return &TenantActor{
		tenantID: tenantID,
		inbox:    make(chan signal.Signal, bufferSize),
	}
}

func (a *TenantActor) Mailbox() chan<- signal.Signal {
	return a.inbox
}

func (a *TenantActor) Start(ctx context.Context) error {
	go func() {
		for {
			select {
			case sig := <-a.inbox:
				a.processSignal(sig)
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (a *TenantActor) Stop() error {
	close(a.inbox)
	return nil
}

func (a *TenantActor) processSignal(sig signal.Signal) {
}
