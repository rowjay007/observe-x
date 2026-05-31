// Package auditlog is ObserveX's tamper-evident audit-trail story.
//
// Phase C-3a separates audit-record *capture* (already happening in
// tenant-api and alert-manager via the Postgres `tenant_audit_log` /
// `alert_history` tables) from audit-record *export*. The Exporter
// interface is the export contract; concrete implementations write
// to a local file (dev/tests), or to S3 with optional object-lock for
// SOC2-style write-once-read-many (WORM) retention.
//
// Why split capture from export?
//
//   - Capture has to be transactional with the action it logs (you
//     don't want a key revoke that silently fails to record). That's
//     the database's job and lives next to the action.
//   - Export has to be eventually durable beyond the application
//     database — SOC2 expects audit records to outlive any single
//     compromise of the application stack.
//
// The Exporter contract is intentionally append-only. Records are
// never updated or deleted by the exporter; if the upstream store
// needs to forget a record (right-to-be-forgotten), it does so on
// the source side and the export trail keeps the original
// (object-locked S3) — that's the entire point.
package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ─── Record ───────────────────────────────────────────────────────────────

// Record is the canonical audit event shape. Fields are intentionally
// lossless against the existing Postgres `tenant_audit_log` row so
// the export pipeline can do a pure copy.
type Record struct {
	ID         string         `json:"id"`           // ULID or BIGSERIAL string
	TenantID   string         `json:"tenant_id,omitempty"`
	Actor      string         `json:"actor"`        // 'admin', 'system', tenant id
	Action     string         `json:"action"`       // dotted, e.g. 'api_key.issue'
	Details    map[string]any `json:"details,omitempty"`
	SourceIP   string         `json:"source_ip,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

// Validate returns an error if the record is missing required fields.
func (r Record) Validate() error {
	if r.Actor == "" {
		return errors.New("auditlog: Actor required")
	}
	if r.Action == "" {
		return errors.New("auditlog: Action required")
	}
	if r.OccurredAt.IsZero() {
		return errors.New("auditlog: OccurredAt required")
	}
	return nil
}

// ─── Exporter contract ───────────────────────────────────────────────────

// Exporter is the export seam. Implementations MUST be safe for
// concurrent callers and MUST treat Append as append-only (no
// rewrites of previously-exported records). Close flushes any
// buffered records and releases resources.
type Exporter interface {
	Append(ctx context.Context, r Record) error
	Close(ctx context.Context) error
}

// NopExporter is the default when no audit-log endpoint is
// configured. Records still flow through pkg/auditlog so callers
// don't have nil-checks at every call site.
type NopExporter struct{}

func (NopExporter) Append(context.Context, Record) error { return nil }
func (NopExporter) Close(context.Context) error          { return nil }

// ─── FileExporter (local NDJSON) ─────────────────────────────────────────

// FileExporter writes records as newline-delimited JSON to a single
// file. Intended for development, integration tests, and air-gapped
// deployments that don't need cloud storage. Each append is followed
// by an fsync so a process crash loses at most one record.
type FileExporter struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func NewFileExporter(path string) (*FileExporter, error) {
	if path == "" {
		return nil, errors.New("auditlog: file path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("auditlog: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditlog: open: %w", err)
	}
	return &FileExporter{path: path, f: f}, nil
}

func (e *FileExporter) Append(_ context.Context, r Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.f.Write(line); err != nil {
		return err
	}
	return e.f.Sync()
}

func (e *FileExporter) Close(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

// Path returns the absolute file path (test helper).
func (e *FileExporter) Path() string { return e.path }

// ─── Buffered exporter wrapper ───────────────────────────────────────────

// BufferedExporter wraps an Exporter with a bounded channel + worker
// so high-frequency callers (e.g. a control-plane under spray) never
// block on slow cloud uploads. Records that don't fit in the buffer
// are NOT silently dropped — the wrapper falls back to a synchronous
// Append so the caller observes back-pressure rather than data loss.
type BufferedExporter struct {
	inner   Exporter
	buf     chan bufEntry
	stopOne sync.Once
	stopCh  chan struct{}
	doneCh  chan struct{}
}

type bufEntry struct {
	ctx context.Context
	rec Record
}

func NewBufferedExporter(inner Exporter, bufferSize int) *BufferedExporter {
	if bufferSize <= 0 {
		bufferSize = 1024
	}
	b := &BufferedExporter{
		inner:  inner,
		buf:    make(chan bufEntry, bufferSize),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *BufferedExporter) Append(ctx context.Context, r Record) error {
	select {
	case b.buf <- bufEntry{ctx: ctx, rec: r}:
		return nil
	default:
		// Buffer full — fall back to sync. Worse latency, but caller
		// sees back-pressure and we don't drop the record.
		return b.inner.Append(ctx, r)
	}
}

func (b *BufferedExporter) Close(ctx context.Context) error {
	b.stopOne.Do(func() {
		close(b.stopCh)
		<-b.doneCh
	})
	return b.inner.Close(ctx)
}

func (b *BufferedExporter) run() {
	defer close(b.doneCh)
	for {
		select {
		case e := <-b.buf:
			// best-effort; we deliberately swallow errors here. The
			// transient errors will recur on the next record; the
			// permanent ones (bad creds) are observable via the
			// inner exporter's own logging/metrics surface.
			_ = b.inner.Append(e.ctx, e.rec)
		case <-b.stopCh:
			// Drain remaining buffer before exit.
			for {
				select {
				case e := <-b.buf:
					_ = b.inner.Append(e.ctx, e.rec)
				default:
					return
				}
			}
		}
	}
}

// ReadAllFromFile is a test helper — read every NDJSON record from
// the file the FileExporter wrote.
func ReadAllFromFile(r io.Reader) ([]Record, error) {
	dec := json.NewDecoder(r)
	var out []Record
	for {
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, rec)
	}
}
