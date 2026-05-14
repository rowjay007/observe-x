package sampling

import (
	"github.com/rowjay007/observe-x/pkg/signal"
	"testing"
)

func TestAdaptiveSamplerKeepsHighScoreTrace(t *testing.T) {
	sampler := NewAdaptiveSampler(0.1, 2)

	sig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id": "trace-high",
			"severity": "ERROR",
		},
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected high-score trace to be kept")
	}
}

func TestAdaptiveSamplerKeepsRecentTrace(t *testing.T) {
	sampler := NewAdaptiveSampler(0.1, 2)

	sig := signal.Signal{
		Type: signal.Trace,
		Attributes: map[string]string{
			"trace_id":    "trace-recent",
			"duration_ms": "2000",
		},
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected first trace to be kept")
	}

	if sampler.Decide(sig) != Keep {
		t.Fatalf("expected second recent trace to be kept")
	}
}
