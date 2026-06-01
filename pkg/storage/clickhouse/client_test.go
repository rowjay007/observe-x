package clickhouse

import (
	"context"
	"reflect"
	"testing"
)

func TestSplitSQLStatements(t *testing.T) {
	t.Parallel()

	in := `-- create metrics
CREATE TABLE IF NOT EXISTS metrics (
    tenant_id String,
    metric_name String
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, metric_name);

-- create logs
CREATE TABLE IF NOT EXISTS logs (
    tenant_id String
) ENGINE = MergeTree();
`

	got := splitSQLStatements(in)
	want := []string{
		"CREATE TABLE IF NOT EXISTS metrics ( tenant_id String, metric_name String ) ENGINE = MergeTree() PARTITION BY toYYYYMMDD(timestamp) ORDER BY (tenant_id, metric_name)",
		"CREATE TABLE IF NOT EXISTS logs ( tenant_id String ) ENGINE = MergeTree()",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected statements:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestOptionsDefaults(t *testing.T) {
	t.Parallel()

	got := Options{}.withDefaults()
	if got.Addr != "localhost:9000" {
		t.Errorf("Addr default: %q", got.Addr)
	}
	if got.BatchSize != 5000 {
		t.Errorf("BatchSize default: %d", got.BatchSize)
	}
	if got.MaxOpenConns != 16 {
		t.Errorf("MaxOpenConns default: %d", got.MaxOpenConns)
	}
	if got.FlushInterval <= 0 {
		t.Errorf("FlushInterval must be positive, got %v", got.FlushInterval)
	}
}

func TestBackendNilSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var b *Backend
	if err := b.Write(ctx, nil); err != nil {
		t.Errorf("nil backend Write should be no-op, got %v", err)
	}
	if err := b.Flush(ctx); err != nil {
		t.Errorf("nil backend Flush should be no-op, got %v", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("nil backend Close should be no-op, got %v", err)
	}
}
