//go:build integration

// query_engine_test exercises the ObserveQL → ClickHouse pipeline
// end-to-end against the live ClickHouse service container provided by
// CI. Skipped automatically when OBSERVE_X_CLICKHOUSE_ADDR is not set.
package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/observeql"
	"github.com/rowjay007/observe-x/pkg/signal"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
)

func TestObserveQLAgainstLiveClickHouse(t *testing.T) {
	addr := os.Getenv("OBSERVE_X_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("OBSERVE_X_CLICKHOUSE_ADDR not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := chstorage.NewBackend(chstorage.Options{
		Addr:           addr,
		Database:       envOr("OBSERVE_X_CLICKHOUSE_DB", "observex"),
		MigrateOnStart: true,
	})
	if err != nil {
		t.Fatalf("backend init: %v", err)
	}
	defer func() { _ = backend.Close() }()

	tenantID := fmt.Sprintf("qt-%d", time.Now().UnixNano())

	// Insert a couple of LOG signals so the planner has something
	// to return.
	now := time.Now().UTC()
	sigs := []signal.Signal{
		{
			TenantID: tenantID, Type: signal.Log,
			Payload:    []byte("hello world"),
			Attributes: map[string]string{"service_name": "api", "severity": "INFO"},
			ReceivedAt: now,
		},
		{
			TenantID: tenantID, Type: signal.Log,
			Payload:    []byte("boom"),
			Attributes: map[string]string{"service_name": "api", "severity": "ERROR"},
			ReceivedAt: now,
		},
	}
	if err := backend.Write(ctx, sigs); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := backend.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// MergeTree visibility is async after Flush; give it a beat.
	time.Sleep(500 * time.Millisecond)

	// Reopen as a raw client for query (executor needs a Client, not Backend).
	client, err := chstorage.NewClient(chstorage.Options{
		Addr:         addr,
		Database:     envOr("OBSERVE_X_CLICKHOUSE_DB", "observex"),
		DialTimeout:  5 * time.Second,
		MaxOpenConns: 4,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = client.Close() }()

	q := `SELECT severity, body FROM logs WHERE severity = "ERROR" SINCE 1h LIMIT 50`
	ast, err := observeql.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := observeql.PlanQuery(ast, observeql.PlannerOptions{TenantID: tenantID})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := client.Query(ctx, plan.SQL, plan.Params...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least 1 row")
	}
	for _, r := range rows {
		if got, _ := r["severity"].(string); got != "ERROR" {
			t.Errorf("row severity = %q want ERROR", got)
		}
		if body, _ := r["body"].(string); !strings.Contains(body, "boom") {
			t.Errorf("body = %q", body)
		}
	}

	// Cleanup: drop the rows for this synthetic tenant.
	t.Cleanup(func() {
		_, _ = client.Query(context.Background(),
			"ALTER TABLE logs DELETE WHERE tenant_id = ?", tenantID)
	})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
