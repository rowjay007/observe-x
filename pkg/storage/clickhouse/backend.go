// Package clickhouse provides the ObserveX ClickHouse storage strategy.
//
// Design (Phase A):
//
//   - The Backend type is the public seam consumed by the processing engine.
//     All higher-level code depends on this package, not on the underlying
//     driver, so we can later add additional StorageBackend implementations
//     (S3 + Parquet, DuckDB) without touching ingest.
//
//   - Writes are decoupled from ClickHouse latency via an internal buffer
//     and a background flusher (every flushInterval or when the buffer
//     reaches batchSize). This is what keeps P99 ingest latency tied to
//     the local WAL and not to whatever ClickHouse happens to be doing.
//
//   - A circuit breaker (sony/gobreaker) wraps every batch insert. When
//     ClickHouse is unhealthy the breaker opens, writes are recorded as
//     dropped (with a labelled Prometheus counter at the engine layer),
//     and the platform keeps accepting ingest. The WAL is the durable
//     source of truth; ClickHouse is a serving layer that can be rebuilt
//     by replaying the WAL (Phase B will wire that replay).
package clickhouse

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
)

//go:embed migrations/001_initial_schema.sql
var initialSchemaSQL string

//go:embed migrations/002_cold_tier.sql
var coldTierMigrationSQL string

// StorageBackend is the interface every storage strategy implements.
// Phase A only ships ClickHouse; Phase C adds an S3 + Parquet cold-tier
// implementation behind the same interface.
type StorageBackend interface {
	Write(ctx context.Context, signals []signal.Signal) error
	Flush(ctx context.Context) error
	Close() error
}

// Options configures the Backend. Zero values yield sane defaults so
// callers can construct with `clickhouse.Options{}` for tests.
type Options struct {
	// Addr is the ClickHouse native protocol address (host:port). Default
	// `localhost:9000`.
	Addr string

	// Database to use. Default `observex`.
	Database string

	// Username / Password — empty means no auth (dev defaults).
	Username string
	Password string

	// BatchSize is the maximum number of signals buffered before a forced
	// flush. Default 5000.
	BatchSize int

	// FlushInterval bounds how long a signal can sit in the buffer before
	// it is flushed. Default 100ms.
	FlushInterval time.Duration

	// DialTimeout is the connection-establishment timeout. Default 5s.
	DialTimeout time.Duration

	// MaxOpenConns caps the connection pool. Default 16.
	MaxOpenConns int

	// MigrateOnStart runs the embedded DDL at startup. Default true.
	MigrateOnStart bool
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = "localhost:9000"
	}
	if o.Database == "" {
		o.Database = "observex"
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 5000
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 100 * time.Millisecond
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 16
	}
	// MigrateOnStart defaults to true; only the explicit `false` opts out.
	return o
}

// Backend is the concrete ClickHouse implementation of StorageBackend.
// It is safe for concurrent use. When the underlying client is nil
// (e.g. ClickHouse unreachable at startup), Write/Flush become no-ops
// and the WAL remains the durable source of truth — callers MUST NOT
// rely on Write for durability.
type Backend struct {
	opts   Options
	client *Client

	mu     sync.Mutex
	buffer []signal.Signal
	closed bool
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewBackend constructs a Backend. If ClickHouse is unreachable at
// startup the backend is returned in *degraded mode* — it accepts
// writes silently so the ingest path stays up. The processing engine
// surfaces the degradation through Prometheus metrics, not by failing
// to start.
func NewBackend(opts Options) (*Backend, error) {
	opts = opts.withDefaults()

	b := &Backend{
		opts:   opts,
		buffer: make([]signal.Signal, 0, opts.BatchSize),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	client, err := NewClient(opts)
	if err != nil {
		// Degraded mode: log via the caller's error handling, but keep going.
		// The engine treats client == nil as "ClickHouse unavailable" and
		// increments a circuit-breaker metric. The WAL still persists.
		close(b.doneCh)
		return b, nil
	}
	b.client = client

	if opts.MigrateOnStart {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.RunMigrations(ctx, initialSchemaSQL); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("clickhouse: migrations failed: %w", err)
		}
		// Phase C-3b: cold-tier ALTERs. RunMigrations is tolerant of
		// the "no such storage policy" failure for single-disk dev
		// clusters; the migration takes effect when storage_policies.xml
		// is mounted (see deploy/clickhouse/storage_policies.xml).
		if err := client.RunMigrations(ctx, coldTierMigrationSQL); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("clickhouse: cold-tier migrations failed: %w", err)
		}
	}

	go b.flushLoop()
	return b, nil
}

// Write enqueues signals for asynchronous batch insertion. It returns
// nil unless the buffer is full AND the immediate flush also fails.
// Callers MUST treat success as "queued for storage" — durability is
// owned by the WAL.
func (b *Backend) Write(ctx context.Context, signals []signal.Signal) error {
	if b == nil || b.client == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("clickhouse: backend closed")
	}

	b.buffer = append(b.buffer, signals...)
	if len(b.buffer) < b.opts.BatchSize {
		return nil
	}
	return b.flushLocked(ctx)
}

// Flush forces immediate write of any buffered signals.
func (b *Backend) Flush(ctx context.Context) error {
	if b == nil || b.client == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	return b.flushLocked(ctx)
}

// Close flushes the buffer, stops the background flusher, and closes
// the underlying connection. It is idempotent.
func (b *Backend) Close() error {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	if b.client != nil {
		close(b.stopCh)
		<-b.doneCh

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.Flush(ctx)
		return b.client.Close()
	}
	return nil
}

// flushLocked must be called with b.mu held. It transfers ownership of
// the buffer to a local slice, releases the lock for the actual write,
// and re-acquires only if the call site needs it. (Here we re-take the
// lock on the deferred path via the caller.)
func (b *Backend) flushLocked(ctx context.Context) error {
	if len(b.buffer) == 0 {
		return nil
	}
	batch := b.buffer
	b.buffer = make([]signal.Signal, 0, b.opts.BatchSize)
	b.mu.Unlock()
	defer b.mu.Lock()

	return b.client.WriteBatch(ctx, batch)
}

func (b *Backend) flushLoop() {
	defer close(b.doneCh)
	t := time.NewTicker(b.opts.FlushInterval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := b.Flush(ctx); err != nil && !isClosed(err) {
				// Soft-fail; metrics carry the loud signal at the engine layer.
				_ = err
			}
			cancel()
		case <-b.stopCh:
			return
		}
	}
}

func isClosed(err error) bool {
	return err != nil && strings.Contains(err.Error(), "closed")
}
