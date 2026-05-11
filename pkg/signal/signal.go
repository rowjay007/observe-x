package signal

import (
	"context"
	"time"
)

type Type string

const (
	Metric  Type = "METRIC"
	Log     Type = "LOG"
	Trace   Type = "TRACE"
	Profile Type = "PROFILE"
)

type Signal struct {
	TenantID   string
	Type       Type
	Payload    []byte
	Attributes map[string]string
	ReceivedAt time.Time
}

type StageFunc func(ctx context.Context, in <-chan Signal) (<-chan Signal, error)
