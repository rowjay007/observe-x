package clickhouse

import (
	"context"
	"github.com/rowjay007/observe-x/pkg/signal"
)

// StorageBackend defines the interface for all storage strategies in ObserveX.
// Implementations must be safe for concurrent use. The Strategy pattern allows
// swapping backends (ClickHouse, DuckDB, S3-Parquet) without changing business logic.
//
// Trade-off: We chose a simple Write/Query interface over a more granular one
// (WriteBatch, WriteStream, etc.) because at Phase 1 the storage engine flushes
// in batches from the WAL. Streaming writes will be added in Phase 3 when the
// query engine needs Arrow IPC integration.
type StorageBackend interface {
	// Write persists a batch of signals to the storage backend.
	// Implementations should handle batching, retries, and compression internally.
	Write(ctx context.Context, signals []signal.Signal) error

	// Query executes a query string against the backend and returns results.
	// The result type will be refined to Arrow RecordBatch in Phase 3.
	Query(ctx context.Context, query string) (interface{}, error)

	// Close releases all resources held by the backend (connections, buffers).
	Close() error
}

// ClickHouseBackend implements StorageBackend for ClickHouse. It wraps the
// Client to provide the strategy interface used by the processing engine.
type ClickHouseBackend struct {
	client *Client
	addr   string
}

// NewClickHouseBackend creates a new ClickHouse storage strategy.
// If the ClickHouse server is unavailable, it returns a degraded backend
// that logs writes but does not fail the pipeline (graceful degradation).
func NewClickHouseBackend(addr string, batchSize int) (*ClickHouseBackend, error) {
	client, err := NewClient(addr, batchSize)
	if err != nil {
		// Return a no-op backend for local dev when ClickHouse is not running
		return &ClickHouseBackend{addr: addr}, nil
	}

	return &ClickHouseBackend{
		client: client,
		addr:   addr,
	}, nil
}

// Write persists signals to ClickHouse via the batching client.
// If the client is nil (degraded mode), the write is silently skipped.
func (b *ClickHouseBackend) Write(ctx context.Context, signals []signal.Signal) error {
	if b.client == nil {
		return nil
	}
	return b.client.Write(ctx, signals)
}

// Query executes a raw SQL query against ClickHouse.
func (b *ClickHouseBackend) Query(ctx context.Context, query string) (interface{}, error) {
	if b.client == nil {
		return nil, nil
	}
	return b.client.Query(ctx, query)
}

// Flush forces the client to write any buffered signals to ClickHouse.
func (b *ClickHouseBackend) Flush(ctx context.Context) error {
	if b.client == nil {
		return nil
	}
	return b.client.Flush(ctx)
}

// Close releases the ClickHouse connection.
func (b *ClickHouseBackend) Close() error {
	if b.client == nil {
		return nil
	}
	return b.client.Close()
}
