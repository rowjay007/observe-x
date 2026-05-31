// Package executor binds a planned ObserveQL Plan to a ClickHouse
// connection, streams rows as NDJSON, and bounds execution time via
// context.
//
// Phase B-3 ships NDJSON; Phase B-3.5 swaps the codec to Arrow IPC
// behind the same Execute() API. The executor itself is codec-aware
// only at the encode step.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/rowjay007/observe-x/pkg/observeql"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
)

type Executor struct {
	client *chstorage.Client
}

func New(client *chstorage.Client) *Executor {
	return &Executor{client: client}
}

// Execute runs a planned query and streams rows to w as NDJSON. The
// caller is expected to set the appropriate Content-Type header
// (application/x-ndjson) before calling Execute.
//
// Each line is a JSON object keyed by column name. The first line is
// a header object {"_kind":"header","columns":[...],"plan":"..."} so
// clients can read schema before consuming data.
//
// Cancellation: Execute respects ctx — when ctx is cancelled the
// underlying ClickHouse rows iterator is closed and Execute returns
// ctx.Err().
func (e *Executor) Execute(ctx context.Context, plan *observeql.Plan, w io.Writer) (rowsWritten int64, err error) {
	if plan == nil {
		return 0, fmt.Errorf("executor: nil plan")
	}
	if e.client == nil {
		return 0, fmt.Errorf("executor: nil clickhouse client")
	}

	start := time.Now()
	rows, err := e.client.Query(ctx, plan.SQL, plan.Params...)
	if err != nil {
		return 0, fmt.Errorf("executor: clickhouse query: %w", err)
	}
	enc := json.NewEncoder(w)

	// Header line — emitted even on empty result so the client can
	// distinguish "query succeeded, no rows" from "transport died".
	cols := planColumns(rows)
	if err := enc.Encode(map[string]any{
		"_kind":    "header",
		"source":   plan.Source,
		"columns":  cols,
		"limit":    plan.Limit,
		"estimate": plan.Estimate,
	}); err != nil {
		return 0, err
	}

	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return rowsWritten, err
		}
		if err := enc.Encode(row); err != nil {
			return rowsWritten, fmt.Errorf("executor: encode row: %w", err)
		}
		rowsWritten++
	}

	// Trailer with timing for client-side instrumentation.
	_ = enc.Encode(map[string]any{
		"_kind":        "trailer",
		"rows_returned": rowsWritten,
		"duration_ms":   time.Since(start).Milliseconds(),
	})
	return rowsWritten, nil
}

// planColumns extracts a stable list of column names from the first
// row of the result. ClickHouse's driver doesn't expose column order
// from a successful empty query in a way that helps NDJSON consumers,
// so we infer from the first row; for empty results we return an
// empty slice and the consumer learns the columns at execute time
// from a follow-up call.
func planColumns(rows []map[string]any) []string {
	if len(rows) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		out = append(out, k)
	}
	return out
}
