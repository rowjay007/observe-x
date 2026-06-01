package parquetexport

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/parquet/file"
)

// staticRows yields a fixed slice and EOFs after.
type staticRows struct {
	rows []map[string]any
	idx  int
}

func (s *staticRows) Next(ctx context.Context) (map[string]any, error) {
	if s.idx >= len(s.rows) {
		return nil, io.EOF
	}
	r := s.rows[s.idx]
	s.idx++
	return r, nil
}

func TestWriteRoundTrip(t *testing.T) {
	src := &staticRows{rows: []map[string]any{
		{"tenant_id": "acme", "value": 1.5, "ts": time.Unix(1700000000, 0)},
		{"tenant_id": "beta", "value": 2.5, "ts": time.Unix(1700000060, 0)},
		{"tenant_id": "gamma", "value": 3.5, "ts": time.Unix(1700000120, 0)},
	}}
	var buf bytes.Buffer
	n, err := Write(context.Background(), src, &buf, Options{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows: %d", n)
	}
	// Read the file back via the low-level Parquet reader and check
	// metadata matches.
	br := bytes.NewReader(buf.Bytes())
	rdr, err := file.NewParquetReader(br)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer rdr.Close()
	if rdr.NumRows() != 3 {
		t.Errorf("file says rows=%d", rdr.NumRows())
	}
	sch := rdr.MetaData().Schema
	// 3 columns: tenant_id (string), ts (ts ns), value (float64).
	// Order is alphabetical from inferSchema.
	cols := []string{}
	for i := 0; i < sch.NumColumns(); i++ {
		cols = append(cols, sch.Column(i).Name())
	}
	want := []string{"tenant_id", "ts", "value"}
	if len(cols) != 3 {
		t.Fatalf("cols: %v", cols)
	}
	// Order may differ — assert as a set, not a sequence.
	set := map[string]bool{}
	for _, c := range cols {
		set[c] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing col %s in %v", w, cols)
		}
	}
}

func TestWriteEmptySource(t *testing.T) {
	src := &staticRows{rows: nil}
	var buf bytes.Buffer
	n, err := Write(context.Background(), src, &buf, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("rows: %d", n)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero bytes for empty source, got %d", buf.Len())
	}
}
