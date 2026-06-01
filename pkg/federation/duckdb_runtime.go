//go:build duckdb

// DuckDB backend — only compiled with `-tags duckdb`.
//
// Requires github.com/marcboeker/go-duckdb (CGO). Operators who
// want federated S3+Parquet reads install DuckDB locally and
// rebuild with the tag:
//
//	go build -tags duckdb ./services/query-engine
//
// Once enabled, the backend opens an in-process DuckDB connection,
// installs the `httpfs` extension, and points it at the cold-tier
// S3 bucket. Queries arriving with source = "cold_<table>" land
// here and run as:
//
//	SELECT … FROM read_parquet('s3://bucket/<prefix>/*.parquet')
//	WHERE …
//
// See ADR-0027.

package federation

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/marcboeker/go-duckdb"
)

type duckdbBackend struct {
	db *sql.DB
}

func NewDuckDBBackend(ctx context.Context, opts DuckDBOptions) (Backend, error) {
	db, err := sql.Open("duckdb", "") // in-memory; the S3 reads go over HTTP
	if err != nil {
		return nil, fmt.Errorf("federation/duckdb: open: %w", err)
	}
	pragmas := []string{"INSTALL httpfs", "LOAD httpfs"}
	if opts.S3Region != "" {
		pragmas = append(pragmas, fmt.Sprintf("SET s3_region='%s'", opts.S3Region))
	}
	if opts.S3Endpoint != "" {
		pragmas = append(pragmas, fmt.Sprintf("SET s3_endpoint='%s'", opts.S3Endpoint))
	}
	if !opts.HTTPSOnly {
		pragmas = append(pragmas, "SET s3_use_ssl=false")
	}
	if opts.MaxMemoryMB > 0 {
		pragmas = append(pragmas, fmt.Sprintf("PRAGMA memory_limit='%dMB'", opts.MaxMemoryMB))
	}
	if opts.Threads > 0 {
		pragmas = append(pragmas, fmt.Sprintf("PRAGMA threads=%d", opts.Threads))
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("federation/duckdb: %s: %w", p, err)
		}
	}
	return &duckdbBackend{db: db}, nil
}

func (d *duckdbBackend) Name() string { return "duckdb-s3" }

func (d *duckdbBackend) Execute(ctx context.Context, query string, params []any) ([]map[string]any, error) {
	rows, err := d.db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("federation/duckdb: query: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
