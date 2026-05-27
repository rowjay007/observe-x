package clickhouse

import (
	"context"
	"github.com/rowjay007/observe-x/pkg/signal"
	innerclickhouse "github.com/rowjay007/observe-x/services/storage-engine/internal/clickhouse"
)

// Backend wraps the internal ClickHouse strategy so higher-level service code
// can opt into storage without depending on an internal package directly.
type Backend struct {
	inner *innerclickhouse.ClickHouseBackend
}

func NewBackend(addr string, batchSize int) (*Backend, error) {
	innerBackend, err := innerclickhouse.NewClickHouseBackend(addr, batchSize)
	if err != nil {
		return nil, err
	}

	return &Backend{inner: innerBackend}, nil
}

func (b *Backend) Write(ctx context.Context, signals []signal.Signal) error {
	if b == nil || b.inner == nil {
		return nil
	}
	return b.inner.Write(ctx, signals)
}

func (b *Backend) Flush(ctx context.Context) error {
	if b == nil || b.inner == nil {
		return nil
	}
	return b.inner.Flush(ctx)
}

func (b *Backend) Close() error {
	if b == nil || b.inner == nil {
		return nil
	}
	return b.inner.Close()
}
