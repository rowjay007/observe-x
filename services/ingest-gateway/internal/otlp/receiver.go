// Package otlp adapts OTLP/HTTP requests to the internal Signal type.
//
// Phase A: payloads are accepted as opaque bytes and tagged with the
// tenant + an `otlp` source attribute. Phase B replaces this passthrough
// with real OTLP protobuf decoding (go.opentelemetry.io/proto/otlp) and
// per-signal-type endpoints (/v1/traces, /v1/metrics, /v1/logs).
package otlp

import (
	"context"
	"errors"
	"time"

	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/signal"
)

// HandleTracePayload pushes a raw OTLP trace payload through the
// processing engine for the given tenant. It is intentionally minimal:
// the caller (HTTP handler) is responsible for tenant resolution.
func HandleTracePayload(ctx context.Context, e *engine.ProcessingEngine, tenantID string, payload []byte) error {
	if e == nil {
		return errors.New("otlp: nil engine")
	}
	if tenantID == "" {
		return errors.New("otlp: missing tenant id")
	}
	if len(payload) == 0 {
		return errors.New("otlp: empty payload")
	}
	sig := signal.Signal{
		TenantID:   tenantID,
		Type:       signal.Trace,
		Payload:    payload,
		Attributes: map[string]string{"source": "otlp"},
		ReceivedAt: time.Now().UTC(),
	}
	return e.ProcessSignal(ctx, sig)
}
