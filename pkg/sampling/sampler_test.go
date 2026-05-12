package sampling

import (
	"testing"
	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestAdaptiveSampler(t *testing.T) {
	sampler := NewAdaptiveSampler(0.1, 100)

	sig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id":     "trace-123",
			"severity":     "ERROR",
			"duration_ms":  "1500",
			"service_name": "payment-service",
		},
	}

	decision := sampler.Decide(sig)
	if decision != Keep {
		t.Errorf("Expected high-value trace to be kept, got %v", decision)
	}

	normalSig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id":     "trace-456",
			"service_name": "logging-service",
		},
	}

	normalDecision := sampler.Decide(normalSig)
	if normalDecision != Drop {
		t.Errorf("Expected low-value trace to be dropped, got %v", normalDecision)
	}
}
