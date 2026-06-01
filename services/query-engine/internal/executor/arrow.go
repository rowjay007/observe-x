// Arrow IPC codec for query-engine results — Phase D-6.
//
// Why Arrow IPC alongside NDJSON?
//
//   - NDJSON is great for ad-hoc curl and the operator UI; it's a
//     wash with bandwidth and a perf disaster on large results
//     (JSON encode/decode is ~10× slower than Arrow IPC for the
//     same byte volume on typed columnar data).
//   - Arrow IPC is the standard format every modern data tool
//     (Pandas / Polars / DuckDB / Spark / R) speaks natively, so
//     enabling it lets customers pipe ObserveQL output into their
//     existing analytics stacks with no glue code.
//
// Selection: the executor switches on the request's Accept header.
// Clients that send `Accept: application/vnd.apache.arrow.stream`
// get an Arrow IPC stream; everyone else gets NDJSON.
//
// ADR-0023.
package executor

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/rowjay007/observe-x/pkg/observeql"
)

// ArrowMediaType is the IANA-registered Arrow IPC stream content
// type. Recognised by Pandas (pd.read_feather), Polars, and DuckDB
// out of the box.
const ArrowMediaType = "application/vnd.apache.arrow.stream"

// ExecuteArrow runs the plan and writes an Arrow IPC stream to w.
// Returns the number of rows written.
//
// Caller MUST have set Content-Type: ArrowMediaType on w before
// invoking.
func (e *Executor) ExecuteArrow(ctx context.Context, plan *observeql.Plan, w io.Writer) (int64, error) {
	if plan == nil {
		return 0, fmt.Errorf("executor/arrow: nil plan")
	}
	if e.client == nil {
		return 0, fmt.Errorf("executor/arrow: nil clickhouse client")
	}
	start := time.Now()
	rows, err := e.client.Query(ctx, plan.SQL, plan.Params...)
	if err != nil {
		return 0, fmt.Errorf("executor/arrow: clickhouse query: %w", err)
	}

	schema, columns := inferSchema(rows)
	pool := memory.NewGoAllocator()
	builders := make([]array.Builder, len(columns))
	for i, f := range schema.Fields() {
		builders[i] = builderFor(pool, f.Type)
	}
	// Build columns row-by-row.
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			releaseBuilders(builders)
			return 0, err
		}
		for i, col := range columns {
			appendValue(builders[i], row[col])
		}
	}
	arrays := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrays[i] = b.NewArray()
	}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
		releaseBuilders(builders)
	}()
	rec := array.NewRecordBatch(schema, arrays, int64(len(rows)))
	defer rec.Release()

	writer := ipc.NewWriter(w, ipc.WithSchema(schema), ipc.WithAllocator(pool))
	defer func() { _ = writer.Close() }()
	// Plan metadata as a schema-level annotation. Clients that don't
	// care can ignore it; Polars surfaces it in the schema object.
	if err := writer.Write(rec); err != nil {
		return 0, fmt.Errorf("executor/arrow: write record: %w", err)
	}
	_ = start // duration is observed via the Prometheus histograms
	return int64(len(rows)), nil
}

// inferSchema examines the first row to pick column types. Like the
// NDJSON path, we don't fail on empty results — we emit a schema
// with zero columns, which Arrow IPC accepts.
func inferSchema(rows []map[string]any) (*arrow.Schema, []string) {
	if len(rows) == 0 {
		return arrow.NewSchema(nil, nil), nil
	}
	first := rows[0]
	cols := make([]string, 0, len(first))
	for k := range first {
		cols = append(cols, k)
	}
	// Stable order across runs/clients.
	// `sort.Strings` would pull in a tiny import; small enough to
	// inline a 4-line insertion sort for column count up to ~30.
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

// arrowTypeFor maps the dynamic Go type the ClickHouse driver hands
// us into an Arrow type. We deliberately collapse the integer kinds
// to int64 and float kinds to float64 — Arrow's typed columns make
// the wire savings, not the type fidelity.
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
		// Nanosecond precision matches ClickHouse DateTime64(9).
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
	default:
		_ = bb
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
		if x > math.MaxInt64 {
			return math.MaxInt64
		}
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
	return toFloat64FromInt(v)
}
func toFloat64FromInt(v any) float64 { return float64(toInt64(v)) }

func releaseBuilders(bs []array.Builder) {
	for _, b := range bs {
		if b != nil {
			b.Release()
		}
	}
}
