// Package spillover gives the supervisor a durable side-car for
// signals that would otherwise be dropped on mailbox-full.
//
// Phase B-4 sized the per-tenant mailbox at 4096 and dropped on
// overflow. That's correct for a single-host, single-deploy
// scenario: a tenant that's flooding deserves back-pressure, and
// the dropped count is exported as a Prometheus metric.
//
// In production we want the back-pressure semantics PLUS a way to
// recover during a brief downstream stall (ClickHouse merge storm,
// network blip, redeploy gap). NATS JetStream is the cheapest
// durable buffer we can lean on without inventing one:
//
//   * Publishers (the supervisor's RouteToTenant) write to a
//     per-tenant subject when the mailbox is full.
//   * Consumers (the same supervisor process, on a separate
//     goroutine) pull from JetStream and re-route into the
//     mailbox at the rate it can accept.
//   * If JetStream itself is unavailable, the spillover degrades
//     to the legacy drop-on-full behaviour — the supervisor keeps
//     running; this package's failure mode is never fatal.
//
// See ADR-0024.
package spillover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rowjay007/observe-x/pkg/signal"
)

// Options configures the JetStream connection + retention.
type Options struct {
	URL        string        // NATS URL, e.g. "nats://nats:4222"
	StreamName string        // default "OBSERVEX_SPILLOVER"
	MaxAge     time.Duration // retention; default 1h
	MaxBytes   int64         // per-stream bound; default 1 GiB
}

func (o Options) withDefaults() Options {
	if o.StreamName == "" {
		o.StreamName = "OBSERVEX_SPILLOVER"
	}
	if o.MaxAge <= 0 {
		o.MaxAge = time.Hour
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 30
	}
	return o
}

// Spillover is the publisher half. Consumers use Consume.
type Spillover struct {
	cfg    Options
	nc     *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream

	publishOK   atomic.Int64
	publishErr  atomic.Int64
	consumeOK   atomic.Int64
	consumeErr  atomic.Int64

	closeOnce sync.Once
}

// New connects to NATS and ensures the spillover stream exists.
// Returns nil + nil error when opts.URL is empty (spillover
// disabled — every Push becomes a no-op so callers don't need
// conditional branches).
func New(ctx context.Context, opts Options) (*Spillover, error) {
	if opts.URL == "" {
		return nil, nil
	}
	opts = opts.withDefaults()
	nc, err := nats.Connect(opts.URL,
		nats.Name("observex-spillover"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.Timeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("spillover: nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("spillover: jetstream init: %w", err)
	}
	// Idempotent: CreateOrUpdate so a redeploy with bumped MaxAge
	// just lands the new config without a manual upgrade dance.
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      opts.StreamName,
		Subjects:  []string{"observex.spillover.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    opts.MaxAge,
		MaxBytes:  opts.MaxBytes,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("spillover: create stream: %w", err)
	}
	return &Spillover{cfg: opts, nc: nc, js: js, stream: stream}, nil
}

// Push best-effort enqueues a signal in JetStream under the
// tenant's subject. Returns an error only on hard publish failure
// (network down + buffer overflow). The caller's right move on
// error is to fall through to the legacy "drop the signal" path.
func (s *Spillover) Push(ctx context.Context, tenantID string, sig signal.Signal) error {
	if s == nil {
		return errors.New("spillover: disabled")
	}
	payload, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("spillover: marshal: %w", err)
	}
	subject := "observex.spillover." + sanitise(tenantID)
	_, err = s.js.Publish(ctx, subject, payload)
	if err != nil {
		s.publishErr.Add(1)
		return fmt.Errorf("spillover: publish: %w", err)
	}
	s.publishOK.Add(1)
	return nil
}

// RouteFn is what the supervisor passes in to Consume; we call it
// for every signal pulled out of the stream. The supervisor's
// implementation is RouteToTenant.
type RouteFn func(tenantID string, sig signal.Signal)

// Consume starts a pull consumer that drains the stream and routes
// every signal back through `route`. Runs until ctx is cancelled.
//
// Durable consumer name fixes the cursor across restarts so we
// pick up where we left off on redeploy.
func (s *Spillover) Consume(ctx context.Context, route RouteFn) error {
	if s == nil {
		return errors.New("spillover: disabled")
	}
	cons, err := s.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Name:          "observex-supervisor",
		Durable:       "observex-supervisor",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "observex.spillover.>",
	})
	if err != nil {
		return fmt.Errorf("spillover: consumer: %w", err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var sig signal.Signal
		if err := json.Unmarshal(msg.Data(), &sig); err != nil {
			s.consumeErr.Add(1)
			_ = msg.Term() // poison message — don't redeliver
			return
		}
		// Subject form: observex.spillover.<tenant>
		tenant := stripPrefix(msg.Subject(), "observex.spillover.")
		route(tenant, sig)
		s.consumeOK.Add(1)
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("spillover: consume: %w", err)
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

// Stats are the lifetime counters; surface via the supervisor's
// /metrics endpoint by reading them in a Prometheus collector hook.
type Stats struct {
	PublishOK  int64
	PublishErr int64
	ConsumeOK  int64
	ConsumeErr int64
}

func (s *Spillover) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		PublishOK:  s.publishOK.Load(),
		PublishErr: s.publishErr.Load(),
		ConsumeOK:  s.consumeOK.Load(),
		ConsumeErr: s.consumeErr.Load(),
	}
}

// Close drains the NATS connection. Idempotent.
func (s *Spillover) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		_ = s.nc.Drain()
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────

// sanitise strips characters NATS subjects don't accept. Subjects
// permit . and > as wildcards so we can't allow them in the tenant
// segment.
func sanitise(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "anon"
	}
	return string(out)
}

func stripPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}
