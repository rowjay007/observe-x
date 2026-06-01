// Package alertsink is the shim between in-process CEP events and the
// out-of-process alert-manager service. It implements actor.EventSink
// with a buffered, fire-and-forget HTTP sender so the actor's hot
// path is never blocked on the alert-manager's availability.
//
// Design:
//
//   - Publish() is non-blocking. If the buffer is full, the event is
//     dropped and a counter is incremented — observability over
//     correctness here, because the same CEP event will fire again
//     on the next signal that crosses the threshold edge.
//   - A worker goroutine drains the buffer and POSTs to
//     {alert-manager}/v1/events with the tenant's API key.
//   - Failed POSTs are retried with exponential backoff (max 3 tries
//     per event); after that the event is dropped + counter bumped.
//
// In tests, the InMemorySink swap point keeps everything synchronous.
package alertsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rowjay007/observe-x/pkg/cep"
)

// HTTPSink is the production EventSink: buffer + worker + retry.
type HTTPSink struct {
	endpoint     string
	apiKey       string
	tenantHeader string

	client *http.Client
	buf    chan cep.Event

	stopOnce sync.Once
	done     chan struct{}

	publishedTotal atomic.Int64
	droppedTotal   atomic.Int64
	failedTotal    atomic.Int64
}

type HTTPOptions struct {
	// Endpoint is the full URL to /v1/events, e.g.
	// "https://alert-manager.observex.svc:7700/v1/events".
	Endpoint string
	// APIKey is the tenant's API key (used as Bearer token).
	APIKey string
	// TenantHeader is what we put in X-Tenant-ID. Usually the tenant
	// owning the actor that produced the event. May be empty if the
	// auth middleware derives tenant from the API key alone.
	TenantHeader string
	// BufferSize bounds the pending-event queue. Default 4096.
	BufferSize int
	// RequestTimeout per HTTP attempt. Default 3s.
	RequestTimeout time.Duration
}

func (o HTTPOptions) withDefaults() HTTPOptions {
	if o.BufferSize <= 0 {
		o.BufferSize = 4096
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 3 * time.Second
	}
	return o
}

// NewHTTPSink starts the worker goroutine. Stop() must be called for
// clean shutdown.
func NewHTTPSink(opts HTTPOptions) *HTTPSink {
	opts = opts.withDefaults()
	s := &HTTPSink{
		endpoint:     opts.Endpoint,
		apiKey:       opts.APIKey,
		tenantHeader: opts.TenantHeader,
		client:       &http.Client{Timeout: opts.RequestTimeout},
		buf:          make(chan cep.Event, opts.BufferSize),
		done:         make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *HTTPSink) Publish(ev cep.Event) {
	select {
	case s.buf <- ev:
		s.publishedTotal.Add(1)
	default:
		s.droppedTotal.Add(1)
	}
}

func (s *HTTPSink) Stop() {
	s.stopOnce.Do(func() {
		close(s.buf)
		<-s.done
	})
}

// Stats returns published/dropped/failed counters for /metrics.
func (s *HTTPSink) Stats() (published, dropped, failed int64) {
	return s.publishedTotal.Load(), s.droppedTotal.Load(), s.failedTotal.Load()
}

func (s *HTTPSink) run() {
	defer close(s.done)
	for ev := range s.buf {
		if err := s.sendWithRetry(ev); err != nil {
			s.failedTotal.Add(1)
		}
	}
}

func (s *HTTPSink) sendWithRetry(ev cep.Event) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 250 * time.Millisecond)
		}
		if err := s.send(ev); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func (s *HTTPSink) send(ev cep.Event) error {
	body, err := json.Marshal(eventToWire(ev))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	tenant := ev.TenantID
	if s.tenantHeader != "" {
		tenant = s.tenantHeader
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("alert-manager: status %d", resp.StatusCode)
	}
	return nil
}

// eventToWire is the canonical CEP→alert-manager translation. The
// rule_id is derived from the event type so the alert-manager can
// dedupe correctly across the same rule firing for the same tenant.
func eventToWire(ev cep.Event) map[string]any {
	severity := "warning"
	if ev.Type == cep.HighErrorRate {
		severity = "critical"
	}
	labels := map[string]string{
		"event_type": string(ev.Type),
	}
	if svc, _ := ev.Data["service"].(string); svc != "" {
		labels["service"] = svc
	}
	return map[string]any{
		"rule_id":     "cep:" + string(ev.Type),
		"severity":    severity,
		"title":       string(ev.Type) + " on " + labels["service"],
		"description": describeEvent(ev),
		"labels":      labels,
		"annotations": ev.Data,
		"occurred_at": ev.Timestamp.UTC().Format(time.RFC3339Nano),
	}
}

func describeEvent(ev cep.Event) string {
	var sb bytes.Buffer
	for k, v := range ev.Data {
		fmt.Fprintf(&sb, "%s=%v ", k, v)
	}
	return sb.String()
}

// ─── InMemorySink for tests ──────────────────────────────────────────────

// InMemorySink is an EventSink that just stores published events in a
// slice. Used in actor tests where we want to assert "this signal
// stream produced this event."
type InMemorySink struct {
	mu     sync.Mutex
	Events []cep.Event
}

func NewInMemorySink() *InMemorySink { return &InMemorySink{} }

func (s *InMemorySink) Publish(ev cep.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, ev)
}

func (s *InMemorySink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Events)
}

func (s *InMemorySink) Snapshot() []cep.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cep.Event, len(s.Events))
	copy(out, s.Events)
	return out
}
