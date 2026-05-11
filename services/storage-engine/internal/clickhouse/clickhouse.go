package clickhouse

import (
	"context"

	"github.com/rowjay007/observe-x/pkg/signal"
)

type StorageBackend interface {
	Write(ctx context.Context, signals []signal.Signal) error
	Query(ctx context.Context, query string) (interface{}, error)
}

type ClickHouseBackend struct {
	addr string
}

func NewClickHouseBackend(addr string) *ClickHouseBackend {
	return &ClickHouseBackend{addr: addr}
}

func (b *ClickHouseBackend) Write(ctx context.Context, signals []signal.Signal) error {
	return nil
}

func (b *ClickHouseBackend) Query(ctx context.Context, query string) (interface{}, error) {
	return nil, nil
}
