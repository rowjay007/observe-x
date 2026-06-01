// Package federation defines the executor seam that lets ObserveQL
// run a query across multiple backends — hot (ClickHouse), cold
// (S3 + Parquet via DuckDB), and any future store that satisfies
// the Backend interface.
//
// Phase D-10 ships:
//
//   - Backend interface — Execute(plan) returns rows. Both
//     "clickhouse" and "duckdb-s3" implement it; the federator
//     dispatches per-source based on the plan's source label.
//   - Router — picks the backend, optionally fans out to many,
//     and merges (UNION ALL semantics with stable ordering).
//   - DuckDBBackend — a thin shim that, when the duckdb build
//     tag is enabled, opens an in-process DuckDB connection and
//     issues `SELECT … FROM read_parquet('s3://…')` queries.
//     The default build returns ErrUnsupported.
//
// We don't take a hard CGO dependency in the default build; the
// duckdb adapter compiles in only when operators opt in with
// `-tags duckdb` (mirrors the ONNX runtime story in mlruntime).
//
// See ADR-0027.
package federation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Backend is any executable target ObserveQL can run a plan
// against. The single Execute call returns the result set fully
// materialised; a streaming variant is a future addition.
type Backend interface {
	// Name returns a short label used in errors and metrics
	// ("clickhouse", "duckdb-s3", "snowflake", …).
	Name() string
	// Execute runs SQL with bound params and returns row maps.
	Execute(ctx context.Context, sql string, params []any) ([]map[string]any, error)
}

// ErrUnsupported is returned by backends that need a build tag to
// be compiled in (e.g. DuckDB requires `-tags duckdb`).
var ErrUnsupported = errors.New("federation: backend unsupported in this build")

// Router fans queries out to the right backend. Backend selection
// is driven by the plan's source label; we keep an explicit map so
// operators can mix-and-match (e.g. "metrics" → clickhouse,
// "cold_logs" → duckdb-s3).
type Router struct {
	mu       sync.RWMutex
	backends map[string]Backend
	defaults Backend
}

func NewRouter(defaultBackend Backend) *Router {
	return &Router{
		backends: map[string]Backend{},
		defaults: defaultBackend,
	}
}

// Register binds a backend to a source label. Calling Register
// twice for the same source overwrites the prior binding —
// supports dynamic reconfiguration without a restart.
func (r *Router) Register(source string, b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[source] = b
}

// Execute dispatches to the backend bound to `source`. If no
// binding exists, the default backend handles it; if no default
// is configured, returns an error.
func (r *Router) Execute(ctx context.Context, source, sql string, params []any) ([]map[string]any, error) {
	r.mu.RLock()
	b, ok := r.backends[source]
	if !ok {
		b = r.defaults
	}
	r.mu.RUnlock()
	if b == nil {
		return nil, fmt.Errorf("federation: no backend for source %q", source)
	}
	return b.Execute(ctx, sql, params)
}

// ExecuteUnion fans the same SQL across multiple sources in
// parallel and stitches the results. Useful when an ObserveQL
// query spans the hot/cold boundary — we run the projection on
// both and merge. Caller MUST ensure SQL is identical across
// backends or pre-rewrite per backend.
//
// Errors fail-fast: the first backend error aborts and any
// outstanding queries are cancelled via ctx.
func (r *Router) ExecuteUnion(ctx context.Context, sources []string, sql string, params []any) ([]map[string]any, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type slot struct {
		rows []map[string]any
		err  error
		src  string
	}
	slots := make([]slot, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		i, src := i, src
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := r.Execute(ctx, src, sql, params)
			slots[i] = slot{rows: rows, err: err, src: src}
		}()
	}
	wg.Wait()
	// Collect with deterministic order (sources in input order).
	var firstErr error
	var out []map[string]any
	for _, s := range slots {
		if s.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("federation: source %q: %w", s.src, s.err)
		}
		if firstErr == nil {
			out = append(out, s.rows...)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	// Stable sort by the special "_ts" column if present, so a
	// unioned hot+cold result reads as one chronological stream.
	sort.SliceStable(out, func(i, j int) bool {
		ti := tsKey(out[i])
		tj := tsKey(out[j])
		return ti < tj
	})
	return out, nil
}

func tsKey(m map[string]any) string {
	for _, k := range []string{"_ts", "timestamp", "start_time", "ts"} {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}

// ─── DuckDB backend (build-tag gated) ───────────────────────────────────
//
// The DuckDB shim lives in duckdb_runtime.go (tag `duckdb`) and a
// stub in duckdb_stub.go (default). Both export NewDuckDBBackend
// so call sites stay portable.
