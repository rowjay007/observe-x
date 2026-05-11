package pipeline

import (
	"context"

	"github.com/rowjay007/observe-x/pkg/signal"
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
