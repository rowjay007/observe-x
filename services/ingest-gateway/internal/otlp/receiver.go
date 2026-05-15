package otlp

import (
	"context"
	"fmt"
	"github.com/rowjay007/observe-x/pkg/signal"
	"sync"
)

type TraceReceiver struct {
	mu         sync.Mutex
	signalChan chan<- signal.Signal
	tenantID   string
}

func NewTraceReceiver(signalChan chan<- signal.Signal, tenantID string) *TraceReceiver {
	return &TraceReceiver{
		signalChan: signalChan,
		tenantID:   tenantID,
	}
}

// Accept receives raw OTLP span data and converts it to signals
func (r *TraceReceiver) Accept(ctx context.Context, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(data) == 0 {
		return nil
	}

	sig := signal.Signal{
		TenantID:   r.tenantID,
		Type:       signal.Trace,
		Payload:    data,
		Attributes: map[string]string{"source": "otlp"},
	}

	select {
	case r.signalChan <- sig:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context cancelled during accept")
	default:
		return fmt.Errorf("signal channel full, dropped trace")
	}
}
