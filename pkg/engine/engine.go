// Package engine is the ObserveX ingest processing pipeline.
//
// Architecture (Phase A):
//
//	┌─────────────────┐  bounded  ┌──────────────┐  bounded  ┌──────────────┐
//	│ ProcessSignal() │──ingestCh─►│ Chain stages │──pipelineOut─►│  worker pool │
//	└─────────────────┘  65536    │ Decode/Valid │   1024    │ (GOMAXPROCS) │
//	                              │ Enrich       │           │              │
//	                              └──────────────┘           └──────┬───────┘
//	                                                                │
//	                                       sampling decision (single owner)
//	                                                                │
//	                              ┌─────────────────────────────────┴──────┐
//	                              │ WAL.Write  (durable, group-committed)  │
//	                              │ Backend.Write (async, batched, breaker)│
//	                              │ Supervisor.Route (per-tenant actor)    │
//	                              └────────────────────────────────────────┘
//
// Back-pressure: ingestCh is bounded. ProcessSignal is non-blocking and
// returns ErrOverloaded if the channel is full — the gateway translates
// this to HTTP 429 / gRPC ResourceExhausted before TCP buffers fill.
//
// Durability: Write to WAL is on the worker's path (not on the
// caller's path). ProcessSignal returns "accepted" once the signal is
// in the channel; the WAL group-commit gives the durability promise
// described in pkg/wal.
//
// Sampling: a single decision (engine) governs whether a trace is
// persisted. Per-tenant actors are responsible only for CEP / event
// emission; they no longer override sampling.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rowjay007/observe-x/pkg/actor"
	"github.com/rowjay007/observe-x/pkg/observability"
	"github.com/rowjay007/observe-x/pkg/sampling"
	"github.com/rowjay007/observe-x/pkg/signal"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
	"github.com/rowjay007/observe-x/pkg/supervisor"
	"github.com/rowjay007/observe-x/pkg/wal"
)

// ErrOverloaded is returned when the engine cannot accept the signal
// without blocking. Callers MUST translate to a back-pressure response
// (HTTP 429 / gRPC ResourceExhausted) so clients can retry/buffer.
var ErrOverloaded = errors.New("processing engine overloaded")

// ─── pipeline stages ──────────────────────────────────────────────────────

// StageFunc is the canonical composable pipeline stage. Stages are
// chained via bounded channels for natural back-pressure propagation.
type StageFunc func(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error)

// Chain composes stages. The output channel of each stage is fed as the
// input to the next; an error from any stage aborts the chain.
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

// DecodeStage validates that the payload is well-formed JSON. Signals
// with invalid payloads are dropped and counted in the dropped metric.
// In Phase B this stage becomes content-type aware (OTLP protobuf vs
// JSON vs StatsD-derived).
func DecodeStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if len(sig.Payload) > 0 {
				var probe json.RawMessage
				if err := json.Unmarshal(sig.Payload, &probe); err != nil {
					observability.SignalsDropped.WithLabelValues(sig.TenantID, string(sig.Type), "decode").Inc()
					continue
				}
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

// ValidateStage ensures signals have a non-empty TenantID. Signals
// without a tenant cannot be routed and are dropped.
func ValidateStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if sig.TenantID == "" {
				observability.SignalsDropped.WithLabelValues("", string(sig.Type), "no_tenant").Inc()
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

// EnrichStage stamps every signal with platform metadata. Geo-IP and
// resource detection are added in Phase B.
func EnrichStage(ctx context.Context, in <-chan signal.Signal) (<-chan signal.Signal, error) {
	out := make(chan signal.Signal, 1024)
	go func() {
		defer close(out)
		for sig := range in {
			if sig.Attributes == nil {
				sig.Attributes = make(map[string]string, 2)
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

// ─── ProcessingEngine ─────────────────────────────────────────────────────

// Options configures the engine. Zero values yield sane defaults.
type Options struct {
	// WALPath is the directory for write-ahead log segments. Required.
	WALPath string

	// SamplingRate is the head-sampling rate for low-score traces.
	// Default 0.1. High-score traces (errors, long durations) are kept
	// regardless of this value.
	SamplingRate float64

	// MaxTraceQueue caps the adaptive sampler's per-process trace
	// memory. Default 1000.
	MaxTraceQueue int

	// IngestBufferSize is the bounded back-pressure point. Default
	// 65536. Once full, ProcessSignal returns ErrOverloaded.
	IngestBufferSize int

	// Workers is the size of the worker pool draining the pipeline.
	// Default GOMAXPROCS.
	Workers int

	// ClickHouse is the storage configuration. If unset, the engine
	// runs in WAL-only mode (no remote persistence). Useful for tests.
	ClickHouse *chstorage.Options
}

func (o Options) withDefaults() Options {
	if o.SamplingRate <= 0 {
		o.SamplingRate = 0.1
	}
	if o.MaxTraceQueue <= 0 {
		o.MaxTraceQueue = 1000
	}
	if o.IngestBufferSize <= 0 {
		o.IngestBufferSize = 65536
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	return o
}

// ProcessingEngine orchestrates the full ingest pipeline.
type ProcessingEngine struct {
	opts Options

	wal     *wal.WAL
	storage *chstorage.Backend
	sup     *supervisor.Supervisor
	sampler *sampling.AdaptiveSampler

	ingestCh chan signal.Signal

	signalsReceived atomic.Int64
	signalsDropped  atomic.Int64
	walWrites       atomic.Int64

	mu      sync.Mutex
	started bool
	stopOnce sync.Once
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewProcessingEngine constructs an engine with default-tuned options.
// Preserved for backwards compatibility with the original Phase 1 API;
// new code SHOULD prefer NewProcessingEngineWithOptions.
func NewProcessingEngine(walPath string, samplingRate float64, maxTraceQueue int) (*ProcessingEngine, error) {
	return NewProcessingEngineWithOptions(Options{
		WALPath:       walPath,
		SamplingRate:  samplingRate,
		MaxTraceQueue: maxTraceQueue,
	})
}

// NewProcessingEngineWithOptions is the explicit constructor.
func NewProcessingEngineWithOptions(opts Options) (*ProcessingEngine, error) {
	opts = opts.withDefaults()
	if opts.WALPath == "" {
		return nil, errors.New("engine: WALPath is required")
	}

	w, err := wal.NewWAL(opts.WALPath)
	if err != nil {
		return nil, err
	}

	var backend *chstorage.Backend
	if opts.ClickHouse != nil {
		backend, err = chstorage.NewBackend(*opts.ClickHouse)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
	}

	return &ProcessingEngine{
		opts:     opts,
		wal:      w,
		storage:  backend,
		sup:      supervisor.NewSupervisor(),
		sampler:  sampling.NewAdaptiveSampler(opts.SamplingRate, opts.MaxTraceQueue),
		ingestCh: make(chan signal.Signal, opts.IngestBufferSize),
	}, nil
}

// Start brings up the supervisor and the worker pool. Idempotent.
func (e *ProcessingEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	e.sup.Start()
	pipeline, err := Chain(runCtx, e.ingestCh, DecodeStage, ValidateStage, EnrichStage)
	if err != nil {
		cancel()
		return err
	}

	for i := 0; i < e.opts.Workers; i++ {
		e.wg.Add(1)
		go e.worker(runCtx, pipeline)
	}

	e.started = true
	return nil
}

// Stop drains the engine and releases resources. Idempotent.
func (e *ProcessingEngine) Stop() error {
	var stopErr error
	e.stopOnce.Do(func() {
		e.mu.Lock()
		started := e.started
		cancel := e.cancel
		e.mu.Unlock()

		if !started {
			if e.wal != nil {
				stopErr = e.wal.Close()
			}
			return
		}

		// Closing the ingest channel makes Chain stages drain.
		close(e.ingestCh)
		if cancel != nil {
			cancel()
		}
		e.wg.Wait()
		e.sup.Stop()

		if e.storage != nil {
			flushCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			_ = e.storage.Flush(flushCtx)
			c()
			_ = e.storage.Close()
		}
		stopErr = e.wal.Close()
	})
	return stopErr
}

// ProcessSignal is the non-blocking entry point. The gateway calls this
// for every inbound signal. Returns ErrOverloaded under back-pressure.
func (e *ProcessingEngine) ProcessSignal(_ context.Context, sig signal.Signal) error {
	e.signalsReceived.Add(1)
	observability.SignalsReceived.WithLabelValues(sig.TenantID, string(sig.Type)).Inc()

	select {
	case e.ingestCh <- sig:
		observability.PipelineQueueDepth.Set(float64(len(e.ingestCh)))
		return nil
	default:
		e.signalsDropped.Add(1)
		observability.SignalsDropped.WithLabelValues(sig.TenantID, string(sig.Type), "overload").Inc()
		return ErrOverloaded
	}
}

// worker drains the post-stage pipeline. The sampling decision and all
// downstream side effects (WAL, storage, actor routing) live here.
func (e *ProcessingEngine) worker(ctx context.Context, pipeline <-chan signal.Signal) {
	defer e.wg.Done()
	for {
		select {
		case sig, ok := <-pipeline:
			if !ok {
				return
			}
			e.handle(ctx, sig)
		case <-ctx.Done():
			return
		}
	}
}

// handle is the per-signal processing step. Visible as
// processSingleSignal for backwards compatibility with existing tests.
func (e *ProcessingEngine) handle(ctx context.Context, sig signal.Signal) {
	// Per-tenant actor routing for CEP & event detection.
	actor := e.sup.GetOrCreateActor(sig.TenantID)
	select {
	case actor.Mailbox() <- sig:
	default:
		observability.SignalsDropped.WithLabelValues(sig.TenantID, string(sig.Type), "actor_full").Inc()
	}

	// Single authoritative sampling decision.
	if !e.shouldPersist(sig) {
		observability.SignalsDropped.WithLabelValues(sig.TenantID, string(sig.Type), "sampled_out").Inc()
		return
	}

	// Durable hop.
	start := time.Now()
	if err := e.wal.Write(sig.Payload); err == nil {
		e.walWrites.Add(1)
		observability.WALWriteLatency.Observe(time.Since(start).Seconds())
	} else {
		observability.SignalsDropped.WithLabelValues(sig.TenantID, string(sig.Type), "wal_error").Inc()
		return
	}

	// Async serving-layer hop. Errors are absorbed by the circuit breaker;
	// the WAL stays the source of truth.
	if e.storage != nil {
		_ = e.storage.Write(ctx, []signal.Signal{sig})
	}
}

// processSingleSignal retained for backward compatibility with the
// engine_test.go that calls it directly. Identical to handle.
func (e *ProcessingEngine) processSingleSignal(ctx context.Context, sig signal.Signal) {
	e.handle(ctx, sig)
}

// shouldPersist applies sampling. Metrics and logs are always kept;
// traces go through the adaptive sampler.
func (e *ProcessingEngine) shouldPersist(sig signal.Signal) bool {
	if sig.Type != signal.Trace {
		return true
	}
	return e.sampler.Decide(sig) == sampling.Keep
}

// Stats returns engine counters. Phase B replaces this with
// metric-registry-backed scraping; we keep it for the existing tests.
func (e *ProcessingEngine) Stats() (received, dropped, walWrites int64) {
	return e.signalsReceived.Load(), e.signalsDropped.Load(), e.walWrites.Load()
}

// SetAlertSink wires a CEP-event sink into every actor (existing
// actors are not retroactively patched — the sink takes effect for
// the next actor created/restarted). Intended to be called once at
// startup, before any traffic, when the alert-manager URL is known.
func (e *ProcessingEngine) SetAlertSink(sink actor.EventSink) {
	e.sup.SetActorOptions(actor.Options{EventSink: sink})
}
