package pipeline

import (
	"context"
	"encoding/json"

	"github.com/rowjay007/observe-x/pkg/signal"
)

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
