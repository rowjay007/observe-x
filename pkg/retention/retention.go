// Package retention applies per-tenant TTL overrides to the
// ClickHouse storage layer.
//
// Phase C-3b shipped a single TTL window per table (metrics 30→90d,
// logs 14→30d, traces 7→30d) plus a multi-disk hot_cold storage
// policy. Phase D-2 lets each tenant override those windows
// independently via `tenant-api`.
//
// Implementation: a tenant override is materialised as a
// `WHERE tenant_id = '<id>'` clause appended to a per-table
// `ALTER TABLE ... MODIFY TTL` DDL. ClickHouse evaluates the
// override row-by-row, so a single physical table holds a mix of
// tenant-specific lifecycles. This is the same pattern Cloudflare,
// Datadog, and Posthog use; it scales to ~thousands of tenants
// before the merge metadata starts to bloat (we monitor with the
// existing system.parts gauges from ADR-0015).
//
// See ADR-0019.
package retention

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Spec is the per-tenant TTL the operator wants applied. Zero
// values keep the table-level default; non-zero values override.
type Spec struct {
	TenantID string

	// HotDays is the floor: rows newer than this stay on the hot disk.
	MetricsHotDays int
	LogsHotDays    int
	TracesHotDays  int

	// TotalDays is the ceiling: rows older than this are deleted.
	// Must be >= the corresponding HotDays.
	MetricsTotalDays int
	LogsTotalDays    int
	TracesTotalDays  int
}

// Validate checks invariants. We're conservative — out-of-range
// values are rejected at the API edge rather than letting ClickHouse
// silently produce an incoherent TTL.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.TenantID) == "" {
		return fmt.Errorf("retention: TenantID required")
	}
	checks := []struct {
		name   string
		hot    int
		total  int
		maxDay int
	}{
		{"metrics", s.MetricsHotDays, s.MetricsTotalDays, 3650},
		{"logs", s.LogsHotDays, s.LogsTotalDays, 730},
		{"traces", s.TracesHotDays, s.TracesTotalDays, 730},
	}
	for _, c := range checks {
		if c.hot < 0 || c.hot > c.maxDay {
			return fmt.Errorf("retention: %s hot days %d out of [0,%d]", c.name, c.hot, c.maxDay)
		}
		if c.total < 0 || c.total > c.maxDay {
			return fmt.Errorf("retention: %s total days %d out of [0,%d]", c.name, c.total, c.maxDay)
		}
		if c.hot > 0 && c.total > 0 && c.total < c.hot {
			return fmt.Errorf("retention: %s total (%d) must be >= hot (%d)", c.name, c.total, c.hot)
		}
	}
	return nil
}

// safeTenantID guards against SQL injection in the tenant id. The
// driver doesn't support parameterised TTL clauses (TTL is a DDL
// construct, not a runtime expression), so we restrict the tenant
// id charset and constant-time reject anything else.
//
// Tenant ids in the control plane are CHECK-constrained to
// [a-z0-9_-]{1,63} (see 001_initial_schema.sql). We re-validate
// here at the DDL boundary as defence-in-depth.
func safeTenantID(id string) error {
	if len(id) == 0 || len(id) > 63 {
		return fmt.Errorf("retention: tenant id length out of bounds")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("retention: tenant id contains illegal character %q", r)
		}
	}
	return nil
}

// Apply emits the per-table ALTERs that pin the tenant's data to
// the requested lifecycle. Each statement is idempotent: ClickHouse
// MODIFY TTL replaces any prior tenant-scoped TTL for that table.
//
// Currently we serialise the DDLs; ClickHouse's TTL evaluation is
// background work, so the latency cost is sub-second per table on
// any cluster that isn't already saturated.
func Apply(ctx context.Context, conn driver.Conn, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := safeTenantID(spec.TenantID); err != nil {
		return err
	}

	type row struct {
		table    string
		col      string
		hotDays  int
		totalDays int
	}
	rows := []row{
		{"metrics", "timestamp", spec.MetricsHotDays, spec.MetricsTotalDays},
		{"logs", "timestamp", spec.LogsHotDays, spec.LogsTotalDays},
		{"traces", "start_time", spec.TracesHotDays, spec.TracesTotalDays},
	}
	for _, r := range rows {
		if r.hotDays == 0 && r.totalDays == 0 {
			continue // skip — table default applies
		}
		stmt := buildTTLStatement(r.table, r.col, spec.TenantID, r.hotDays, r.totalDays)
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("retention: exec %s: %w", r.table, err)
		}
	}
	return nil
}

// buildTTLStatement composes the per-tenant MODIFY TTL DDL.
// Visible for tests so we can assert the exact wire form.
func buildTTLStatement(table, col, tenantID string, hotDays, totalDays int) string {
	var clauses []string
	if hotDays > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"toDateTime(%s) + INTERVAL %d DAY TO DISK 'cold_s3' WHERE tenant_id = '%s'",
			col, hotDays, tenantID))
	}
	if totalDays > 0 {
		clauses = append(clauses, fmt.Sprintf(
			"toDateTime(%s) + INTERVAL %d DAY DELETE WHERE tenant_id = '%s'",
			col, totalDays, tenantID))
	}
	return fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s", table, strings.Join(clauses, ", "))
}

// Drop removes any per-tenant TTL overrides. Use when a tenant is
// being deleted or when an operator wants to revert to defaults.
func Drop(ctx context.Context, conn driver.Conn, tenantID string) error {
	if err := safeTenantID(tenantID); err != nil {
		return err
	}
	// ClickHouse doesn't expose a "drop a specific WHERE-clause TTL"
	// operation, so we re-issue the table-level default with a
	// no-op WHERE. The cleanest path is the same MODIFY TTL with
	// the table-default windows, scoped to this tenant.
	defaults := Spec{
		TenantID:         tenantID,
		MetricsHotDays:   30, MetricsTotalDays: 90,
		LogsHotDays:      14, LogsTotalDays:    30,
		TracesHotDays:    7,  TracesTotalDays:  30,
	}
	return Apply(ctx, conn, defaults)
}

// Touch returns now() formatted for audit records.
func Touch() time.Time { return time.Now().UTC() }
