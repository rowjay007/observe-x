// Package parquetexport materialises ObserveQL result sets as
// Parquet files in S3 so they can be consumed by Athena, Trino,
// DuckDB, or Spark without touching ClickHouse.
//
// Phase D-8 (ADR-0025). The flow:
//
//   1. Caller hands us a query plan (or a raw SELECT).
//   2. We stream the rows through the same Arrow record builders
//      that pkg/.../executor/arrow.go uses, then write a Parquet
//      file (Snappy compression, 64K row groups) to an io.Writer.
//   3. For S3, the caller wraps with the AWS SDK's s3manager
//      uploader; for tests/local, an os.File is fine.
//
// We deliberately don't bake S3 in here — the package returns
// bytes/writes to an io.Writer so the orchestration (which bucket,
// which key, what retry policy) is the caller's responsibility.
package parquetexport

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// Options tunes the writer; all fields are optional.
type Options struct {
	// RowGroupSize is the number of rows per Parquet row group.
	// Default 64K — a sweet spot for Athena/Trino predicate
	// pushdown and Snappy compression efficiency.
	RowGroupSize int64
	// Compression is the column codec. Default Snappy because
	// every consumer can read it; Zstd is ~30% smaller but more
	// CPU. Operators can override.
	Compression compress.Compression
}

func (o Options) withDefaults() Options {
	if o.RowGroupSize <= 0 {
		o.RowGroupSize = 64 * 1024
	}
	if o.Compression == 0 {
		o.Compression = compress.Codecs.Snappy
	}
	return o
}

// RowSource is what the writer iterates. Returns one row map per
// call and (nil, io.EOF) when exhausted. Lets callers stream from
// ClickHouse without buffering the whole result set.
type RowSource interface {
	Next(ctx context.Context) (map[string]any, error)
}

// Write streams rows from src into Parquet, written to w. Returns
// the row count it wrote.
func Write(ctx context.Context, src RowSource, w io.Writer, opts Options) (int64, error) {
	opts = opts.withDefaults()
	// Peek at the first row to learn the schema. Arrow IPC accepts
	// empty results; Parquet doesn't have a "schema-only" output, so
	// we treat an empty source as success-with-zero-rows.
	first, err := src.Next(ctx)
	if err == io.EOF {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("parquetexport: first row: %w", err)
	}
	schema, cols := inferSchema(first)
	pool := memory.NewGoAllocator()
	builders := make([]array.Builder, len(cols))
	for i, f := range schema.Fields() {
		builders[i] = builderFor(pool, f.Type)
	}
	defer func() {
		for _, b := range builders {
			if b != nil {
				b.Release()
			}
		}
	}()

	pw, err := pqarrow.NewFileWriter(schema, w,
		parquet.NewWriterProperties(parquet.WithCompression(opts.Compression)),
		pqarrow.NewArrowWriterProperties())
	if err != nil {
		return 0, fmt.Errorf("parquetexport: writer: %w", err)
	}
	defer pw.Close()

	var totalRows int64
	var rowsInRG int64

	appendRow := func(row map[string]any) {
		for i, col := range cols {
			appendValue(builders[i], row[col])
		}
		totalRows++
		rowsInRG++
	}
	appendRow(first)

	flush := func() error {
		if rowsInRG == 0 {
			return nil
		}
		arrays := make([]arrow.Array, len(builders))
		for i, b := range builders {
			arrays[i] = b.NewArray()
		}
		defer func() {
			for _, a := range arrays {
				a.Release()
			}
		}()
		rec := array.NewRecord(schema, arrays, rowsInRG)
		defer rec.Release()
		if err := pw.WriteBuffered(rec); err != nil {
			return fmt.Errorf("parquetexport: write rg: %w", err)
		}
		// Reset builders for the next row group.
		for i, f := range schema.Fields() {
			builders[i].Release()
			builders[i] = builderFor(pool, f.Type)
		}
		rowsInRG = 0
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return totalRows, err
		}
		row, err := src.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return totalRows, fmt.Errorf("parquetexport: next: %w", err)
		}
		appendRow(row)
		if rowsInRG >= opts.RowGroupSize {
			if err := flush(); err != nil {
				return totalRows, err
			}
		}
	}
	if err := flush(); err != nil {
		return totalRows, err
	}
	return totalRows, nil
}

// ─── schema / type helpers (duplicated from the executor Arrow path
//     because the executor lives in a service-internal package; the
//     duplication is intentional — we don't want a cycle and the
//     helper code is < 60 lines).

func inferSchema(first map[string]any) (*arrow.Schema, []string) {
	cols := make([]string, 0, len(first))
	for k := range first {
		cols = append(cols, k)
	}
	for i := 1; i < len(cols); i++ {
		for j := i; j > 0 && cols[j-1] > cols[j]; j-- {
			cols[j-1], cols[j] = cols[j], cols[j-1]
		}
	}
	fields := make([]arrow.Field, len(cols))
	for i, c := range cols {
		fields[i] = arrow.Field{Name: c, Type: arrowTypeFor(first[c]), Nullable: true}
	}
	return arrow.NewSchema(fields, nil), cols
}

func arrowTypeFor(v any) arrow.DataType {
	switch v.(type) {
	case bool:
		return arrow.FixedWidthTypes.Boolean
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return arrow.PrimitiveTypes.Int64
	case float32, float64:
		return arrow.PrimitiveTypes.Float64
	case time.Time:
		return &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: "UTC"}
	default:
		return arrow.BinaryTypes.String
	}
}

func builderFor(pool memory.Allocator, t arrow.DataType) array.Builder {
	switch t.ID() {
	case arrow.BOOL:
		return array.NewBooleanBuilder(pool)
	case arrow.INT64:
		return array.NewInt64Builder(pool)
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool)
	case arrow.TIMESTAMP:
		return array.NewTimestampBuilder(pool, t.(*arrow.TimestampType))
	default:
		return array.NewStringBuilder(pool)
	}
}

func appendValue(b array.Builder, v any) {
	if v == nil {
		b.AppendNull()
		return
	}
	switch bb := b.(type) {
	case *array.BooleanBuilder:
		if x, ok := v.(bool); ok {
			bb.Append(x)
		} else {
			bb.AppendNull()
		}
	case *array.Int64Builder:
		bb.Append(toInt64(v))
	case *array.Float64Builder:
		bb.Append(toFloat64(v))
	case *array.TimestampBuilder:
		if t, ok := v.(time.Time); ok {
			bb.Append(arrow.Timestamp(t.UnixNano()))
		} else {
			bb.AppendNull()
		}
	case *array.StringBuilder:
		bb.Append(fmt.Sprint(v))
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	}
	return 0
}

func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float32:
		return float64(x)
	case float64:
		return x
	}
	return float64(toInt64(v))
}
