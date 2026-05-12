package engine

import (
	"context"
	"encoding/json"

	"github.com/rowjay007/observe-x/pkg/sampling"
	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/supervisor"
	"github.com/rowjay007/observe-x/pkg/wal"
)

type StageFunc func(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error)

func Chain(ctx context.Context, in <-chan signal.Signal, stages ...StageFunc) (<-chan signal.Signal, error) {
	current := in
	for _, stage := range stages {
		next, err := stage(ctx, current)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func DecodeStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			var decoded map[string]interface{}
			if err := json.Unmarshal(sig.Payload, &decoded); err != nil {
				continue
			}
			select {
			case out <- sig:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func ValidateStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if sig.TenantID == "" {
				continue
			}
			select {
			case out <- sig:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func EnrichStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if sig.Attributes == nil {
				sig.Attributes = make(map[string]string)
			}
			sig.Attributes["observex.version"] = "1.0.0"
			select {
			case out <- sig:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

type ProcessingEngine struct {
	walInstance   *wal.WAL
	supervisor    *supervisor.Supervisor
	samplingRate  float64
	maxTraceQueue int
}

func NewProcessingEngine(walPath string, samplingRate float64, maxTraceQueue int) (*ProcessingEngine, error) {
	walInstance, err := wal.NewWAL(walPath)
	if err != nil {
		return nil, err
	}

	return &ProcessingEngine{
		walInstance:   walInstance,
		supervisor:    supervisor.NewSupervisor(),
		samplingRate:  samplingRate,
		maxTraceQueue: maxTraceQueue,
	}, nil
}

func (e *ProcessingEngine) Start(ctx context.Context) error {
	e.supervisor.Start()
	return nil
}

func (e *ProcessingEngine) Stop() error {
	e.supervisor.Stop()
	return e.walInstance.Close()
}

func (e *ProcessingEngine) ProcessSignal(ctx context.Context, sig signal.Signal) error {
	actor := e.supervisor.GetOrCreateActor(sig.TenantID)

	actor.Mailbox() <- sig

	if e.shouldPersist(sig) {
		return e.walInstance.Write(sig.Payload)
	}

	return nil
}

func (e *ProcessingEngine) shouldPersist(sig signal.Signal) bool {
	if sig.Type != signal.Trace {
		return true
	}

	sampler := sampling.NewAdaptiveSampler(e.samplingRate, e.maxTraceQueue)
	decision := sampler.Decide(sig)

	return decision == sampling.Keep
}
