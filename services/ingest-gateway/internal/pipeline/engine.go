package pipeline

import (
	"context"
	"time"
)

type SignalType string

const (
	Metric  SignalType = "METRIC"
	Log     SignalType = "LOG"
	Trace   SignalType = "TRACE"
	Profile SignalType = "PROFILE"
)

type Signal struct {
	TenantID   string
	Type       SignalType
	Payload    []byte
	Attributes map[string]string
	ReceivedAt time.Time
}

type StageFunc func(ctx context.Context, in <-chan Signal) (<-chan Signal, error)

func Chain(ctx context.Context, in <-chan Signal, stages ...StageFunc) (<-chan Signal, error) {
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
