package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rowjay007/observe-x/pkg/sampling"
	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/supervisor"
	"github.com/rowjay007/observe-x/pkg/wal"
	storageclickhouse "github.com/rowjay007/observe-x/services/storage-engine/clickhouse"
)

// ErrOverloaded is returned when the processing engine's internal buffers
// are saturated and the signal cannot be accepted. The caller should apply
// load-shedding (e.g. HTTP 429 or gRPC ResourceExhausted).
var ErrOverloaded = errors.New("processing engine overloaded")

// ─── Pipeline Stage Types ──────────────────────────────────────────────────

// StageFunc represents a composable pipeline stage. Each stage reads from an
// input channel and produces an output channel. Stages are chained via bounded
// channels so that back-pressure propagates naturally upstream.
type StageFunc func(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error)

// Chain composes multiple pipeline stages into a single pipeline. If any stage
// returns an error during setup, the entire chain is aborted.
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

// ─── Built-in Pipeline Stages ──────────────────────────────────────────────

// DecodeStage validates that the payload is well-formed JSON. Signals with
// invalid payloads are silently dropped (logged in production). The bounded
// output channel (1024) provides natural back-pressure.
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

// ValidateStage ensures signals have a non-empty TenantID. Signals without
// a tenant are dropped since they cannot be routed to an actor.
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

// EnrichStage adds system-level metadata to every signal (version tag,
// ingestion timestamp). This is the place to add geo-lookup, resource
// detection, or tag normalization in future phases.
func EnrichStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if sig.Attributes == nil {
				sig.Attributes = make(map[string]string)
			}
			sig.Attributes["observex.version"] = "1.0.0"
			sig.Attributes["observex.ingested_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			select {
			case out <- sig:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// ─── Processing Engine ─────────────────────────────────────────────────────

// ProcessingEngine orchestrates the full ingest pipeline: signal reception,
// actor routing, adaptive sampling, WAL persistence, and background flushing
// to the downstream storage backend (ClickHouse).
//
// Back-pressure design:
//   - The ingestChan is a bounded channel (default 65536). When it fills up,
//     ProcessSignal returns ErrOverloaded so the gateway can shed load (429).
//   - Each pipeline stage uses bounded channels (1024) for natural flow control.
//   - The WAL write is synchronous on the hot path for durability guarantees.
type ProcessingEngine struct {
	walInstance    *wal.WAL
	storageBackend *storageclickhouse.Backend
	supervisor     *supervisor.Supervisor
	sampler        *sampling.AdaptiveSampler
	samplingRate   float64
	maxTraceQueue  int

	// ingestChan is the bounded entry point for all signals. When full,
	// new signals are rejected with ErrOverloaded (load shedding).
	ingestChan chan signal.Signal

	// Metrics for self-observability
	signalsReceived atomic.Int64
	signalsDropped  atomic.Int64
	walWrites       atomic.Int64

	mu      sync.Mutex
	started bool
}

// NewProcessingEngine creates a new engine with the given WAL directory,
// sampling rate (0.0–1.0), and max trace queue size.
func NewProcessingEngine(walPath string, samplingRate float64, maxTraceQueue int) (*ProcessingEngine, error) {
	walInstance, err := wal.NewWAL(walPath)
	if err != nil {
		return nil, err
	}

	storageAddr := os.Getenv("OBSERVE_X_CLICKHOUSE_ADDR")
	if storageAddr == "" {
		storageAddr = "localhost:9000"
	}
	storageBackend, _ := storageclickhouse.NewBackend(storageAddr, 1000)

	return &ProcessingEngine{
		walInstance:    walInstance,
		storageBackend: storageBackend,
		supervisor:     supervisor.NewSupervisor(),
		sampler:        sampling.NewAdaptiveSampler(samplingRate, maxTraceQueue),
		samplingRate:   samplingRate,
		maxTraceQueue:  maxTraceQueue,
		ingestChan:     make(chan signal.Signal, 65536),
	}, nil
}

// Start initializes the supervisor and begins the background pipeline
// consumer goroutine that drains ingestChan through the pipeline stages.
func (e *ProcessingEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.started {
		return nil
	}

	e.supervisor.Start()

	pipeline, err := Chain(ctx, e.ingestChan, DecodeStage, ValidateStage, EnrichStage)
	if err != nil {
		return err
	}

	// Start the background pipeline consumer
	go func() {
		for sig := range pipeline {
			e.processSingleSignal(ctx, sig)
		}
	}()

	e.started = true
	return nil
}

// Stop gracefully shuts down the supervisor and closes the WAL.
func (e *ProcessingEngine) Stop() error {
	e.supervisor.Stop()
	close(e.ingestChan)

	if e.storageBackend != nil {
		_ = e.storageBackend.Flush(context.Background())
		_ = e.storageBackend.Close()
	}

	return e.walInstance.Close()
}

// ProcessSignal is the main entry point for all inbound signals. It applies
// load shedding: if the internal buffer is full, it returns ErrOverloaded
// immediately instead of blocking. This ensures the gateway can return 429
// to clients before TCP buffers fill up.
func (e *ProcessingEngine) ProcessSignal(ctx context.Context, sig signal.Signal) error {
	e.signalsReceived.Add(1)

	// Non-blocking send: if the channel is full, shed load
	select {
	case e.ingestChan <- sig:
		// Signal accepted into the pipeline
	default:
		e.signalsDropped.Add(1)
		return ErrOverloaded
	}

	return nil
}

// runPipeline is the background goroutine that reads from ingestChan,
// routes each signal to the appropriate tenant actor, applies adaptive
// sampling for traces, and persists to the WAL.
func (e *ProcessingEngine) runPipeline(ctx context.Context) {
	for {
		select {
		case sig, ok := <-e.ingestChan:
			if !ok {
				return
			}
			e.processSingleSignal(ctx, sig)
		case <-ctx.Done():
			// Drain remaining signals before exiting
			for sig := range e.ingestChan {
				e.processSingleSignal(ctx, sig)
			}
			return
		}
	}
}

// processSingleSignal handles one signal: route to actor + persist to WAL.
func (e *ProcessingEngine) processSingleSignal(ctx context.Context, sig signal.Signal) {
	// Route to tenant actor for CEP, aggregation, and enrichment
	actor := e.supervisor.GetOrCreateActor(sig.TenantID)
	select {
	case actor.Mailbox() <- sig:
	default:
		// Actor mailbox full — drop the signal for this tenant
		e.signalsDropped.Add(1)
	}

	// Persist to WAL (synchronous for durability)
	if e.shouldPersist(sig) {
		if err := e.walInstance.Write(sig.Payload); err == nil {
			e.walWrites.Add(1)
		}
	}

	if e.storageBackend != nil {
		_ = e.storageBackend.Write(ctx, []signal.Signal{sig})
	}
}

// shouldPersist determines whether a signal should be written to the WAL.
// All metrics and logs are always persisted. Traces are subject to adaptive
// sampling — only high-value traces (errors, high latency) are guaranteed
// to be kept.
func (e *ProcessingEngine) shouldPersist(sig signal.Signal) bool {
	if sig.Type != signal.Trace {
		return true
	}
	decision := e.sampler.Decide(sig)
	return decision == sampling.Keep
}

// ─── Observability Getters ─────────────────────────────────────────────────

// Stats returns current engine metrics for self-observability.
func (e *ProcessingEngine) Stats() (received, dropped, walWrites int64) {
	return e.signalsReceived.Load(), e.signalsDropped.Load(), e.walWrites.Load()
}
