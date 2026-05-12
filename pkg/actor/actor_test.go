package actor

import (
	"context"
	"testing"
	"time"
	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestTenantActor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	actor := NewTenantActor("test-tenant", 10)
	err := actor.Start(ctx)
	if err != nil {
		t.Fatalf("Failed to start actor: %v", err)
	}

	sig := signal.Signal{
		Type: signal.Log,
		Attributes: map[string]string{
			"severity":     "ERROR",
			"service_name": "api",
		},
	}

	actor.Mailbox() <- sig

	time.Sleep(100 * time.Millisecond)

	err = actor.Stop()
	if err != nil {
		t.Errorf("Failed to stop actor: %v", err)
	}
}
