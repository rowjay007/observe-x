//go:build !duckdb

package federation

import "context"

// DuckDBOptions configures the S3 + DuckDB backend. Mirrored in
// both build variants so consumer code is portable.
type DuckDBOptions struct {
	// S3Region for the cold-tier bucket.
	S3Region string
	// S3Endpoint overrides the default (useful for MinIO / R2).
	S3Endpoint string
	// HTTPSOnly forces SSL on the S3 client.
	HTTPSOnly bool
	// MaxMemoryMB caps DuckDB's process memory.
	MaxMemoryMB int
	// Threads — pinned worker count. 0 = auto.
	Threads int
}

// NewDuckDBBackend in the default build returns ErrUnsupported.
// Compile with `-tags duckdb` (and a working DuckDB CGO toolchain)
// to enable the real adapter.
func NewDuckDBBackend(_ context.Context, _ DuckDBOptions) (Backend, error) {
	return nil, ErrUnsupported
}
