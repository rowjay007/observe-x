// Package notifier abstracts the act of "tell a human about this
// alert." Concrete implementations (Slack, PagerDuty, generic
// Webhook) live alongside this file. The Dispatcher fans out a single
// Notification to multiple notifiers in parallel with bounded
// concurrency, returning the first error if any fail.
//
// Notifications are intentionally NOT idempotent at the notifier
// level — that's the alert-manager's responsibility (it dedupes
// against Postgres before calling Dispatch). The notifier's job is
// "deliver this exact payload right now or surface an error."
package notifier

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Severity mirrors sloburn.Severity at the wire layer so importers
// don't need to depend on sloburn.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Notification is the wire shape carried to every notifier.
type Notification struct {
	// Fingerprint uniquely identifies the alert (tenant + SLO + rule).
	// Used by upstream dedupers; notifiers should NOT mutate it.
	Fingerprint string

	TenantID    string
	Severity    Severity
	Title       string
	Description string
	Source      string // e.g. "alert-manager", "cep"
	Labels      map[string]string
	Annotations map[string]string

	StartsAt time.Time
	// EndsAt zero ⇒ alert is firing; non-zero ⇒ resolved.
	EndsAt time.Time
}

// Resolved is shorthand for "EndsAt is non-zero and in the past."
func (n Notification) Resolved() bool { return !n.EndsAt.IsZero() && n.EndsAt.Before(time.Now()) }

// Notifier delivers Notifications to a single destination.
// Implementations MUST respect ctx for cancellation/timeout. Send is
// the canonical method; Name is used in logs and metrics.
type Notifier interface {
	Name() string
	Send(ctx context.Context, n Notification) error
}

// ─── Dispatcher ───────────────────────────────────────────────────────────

// Dispatcher fans a Notification out to many Notifiers in parallel.
// Concurrency is bounded by MaxParallel (default GOMAXPROCS). The
// dispatcher honours per-notifier timeouts (PerNotifierTimeout,
// default 5s) so a hanging downstream cannot stall the whole fan-out.
type Dispatcher struct {
	notifiers          []Notifier
	maxParallel        int
	perNotifierTimeout time.Duration
}

type DispatcherOption func(*Dispatcher)

func WithMaxParallel(n int) DispatcherOption {
	return func(d *Dispatcher) { d.maxParallel = n }
}
func WithPerNotifierTimeout(t time.Duration) DispatcherOption {
	return func(d *Dispatcher) { d.perNotifierTimeout = t }
}

func NewDispatcher(notifiers []Notifier, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{
		notifiers:          notifiers,
		maxParallel:        len(notifiers),
		perNotifierTimeout: 5 * time.Second,
	}
	for _, o := range opts {
		o(d)
	}
	if d.maxParallel < 1 {
		d.maxParallel = 1
	}
	return d
}

// Dispatch fans n out to every registered notifier. Returns the
// joined errors (one per notifier that failed), or nil on full
// success.
func (d *Dispatcher) Dispatch(ctx context.Context, n Notification) error {
	if len(d.notifiers) == 0 {
		return nil
	}
	sem := make(chan struct{}, d.maxParallel)
	errCh := make(chan error, len(d.notifiers))

	var wg sync.WaitGroup
	for _, nf := range d.notifiers {
		wg.Add(1)
		nfLocal := nf
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c, cancel := context.WithTimeout(ctx, d.perNotifierTimeout)
			defer cancel()
			if err := nfLocal.Send(c, n); err != nil {
				errCh <- &NotifierError{Name: nfLocal.Name(), Err: err}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var collected []error
	for e := range errCh {
		collected = append(collected, e)
	}
	if len(collected) == 0 {
		return nil
	}
	return errors.Join(collected...)
}

// Names returns the registered notifier names (for /health, logs).
func (d *Dispatcher) Names() []string {
	out := make([]string, 0, len(d.notifiers))
	for _, n := range d.notifiers {
		out = append(out, n.Name())
	}
	return out
}

// ─── Error helpers ────────────────────────────────────────────────────────

type NotifierError struct {
	Name string
	Err  error
}

func (e *NotifierError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *NotifierError) Unwrap() error { return e.Err }
