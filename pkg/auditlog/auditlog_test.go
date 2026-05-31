package auditlog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRecordValidate(t *testing.T) {
	cases := []struct {
		name    string
		r       Record
		wantErr bool
	}{
		{name: "missing actor", r: Record{Action: "x", OccurredAt: time.Now()}, wantErr: true},
		{name: "missing action", r: Record{Actor: "x", OccurredAt: time.Now()}, wantErr: true},
		{name: "missing occurredAt", r: Record{Actor: "x", Action: "y"}, wantErr: true},
		{name: "valid", r: Record{Actor: "x", Action: "y", OccurredAt: time.Now()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.r.Validate()
			if c.wantErr && err == nil {
				t.Error("expected error")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected: %v", err)
			}
		})
	}
}

func TestFileExporterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir/audit.ndjson")
	e, err := NewFileExporter(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	records := []Record{
		{ID: "1", Actor: "admin", Action: "tenant.create", OccurredAt: now, Details: map[string]any{"tier": "pro"}},
		{ID: "2", Actor: "system", Action: "api_key.revoke", OccurredAt: now},
	}
	for _, r := range records {
		if err := e.Append(context.Background(), r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := ReadAllFromFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[0].Action != "tenant.create" || got[1].Action != "api_key.revoke" {
		t.Errorf("order/content lost: %+v", got)
	}
}

func TestFileExporterRejectsInvalidRecord(t *testing.T) {
	dir := t.TempDir()
	e, err := NewFileExporter(filepath.Join(dir, "x.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(context.Background())
	if err := e.Append(context.Background(), Record{Action: "x"}); err == nil {
		t.Error("expected validate error")
	}
}

func TestNopExporterNeverErrors(t *testing.T) {
	var e NopExporter
	if err := e.Append(context.Background(), Record{}); err != nil {
		t.Errorf("nop append should never error: %v", err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Errorf("nop close should never error: %v", err)
	}
}

// fakeExporter records every Append for assertions.
type fakeExporter struct {
	mu  sync.Mutex
	got []Record
}

func (f *fakeExporter) Append(_ context.Context, r Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r)
	return nil
}
func (f *fakeExporter) Close(context.Context) error { return nil }
func (f *fakeExporter) snapshot() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.got...)
}

func TestBufferedExporterDrainsOnClose(t *testing.T) {
	inner := &fakeExporter{}
	b := NewBufferedExporter(inner, 16)

	for i := 0; i < 10; i++ {
		if err := b.Append(context.Background(), Record{
			Actor: "a", Action: "x", OccurredAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inner.snapshot(); len(got) != 10 {
		t.Fatalf("expected 10 drained; got %d", len(got))
	}
}

// slowExporter delays each Append for the given duration so we can
// observe the buffered wrapper's behaviour under back-pressure.
type slowExporter struct {
	delay time.Duration
	count int
	mu    sync.Mutex
}

func (s *slowExporter) Append(_ context.Context, _ Record) error {
	time.Sleep(s.delay)
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	return nil
}
func (s *slowExporter) Close(context.Context) error { return nil }

func TestBufferedExporterNeverDropsUnderBackpressure(t *testing.T) {
	// Buffer size 4, inner takes 25ms each. A burst of 20 records
	// will fill the buffer; the rest must take the synchronous
	// fallback. None should be lost.
	inner := &slowExporter{delay: 25 * time.Millisecond}
	b := NewBufferedExporter(inner, 4)

	const burst = 20
	for i := 0; i < burst; i++ {
		if err := b.Append(context.Background(), Record{
			Actor: "a", Action: "x", OccurredAt: time.Now(),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	inner.mu.Lock()
	got := inner.count
	inner.mu.Unlock()
	if got != burst {
		t.Fatalf("expected %d total records persisted, got %d", burst, got)
	}
}
