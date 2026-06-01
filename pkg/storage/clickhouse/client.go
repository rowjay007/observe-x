package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/sony/gobreaker"

	"github.com/rowjay007/observe-x/pkg/signal"
)

// Client wraps the ClickHouse native v2 driver with a circuit breaker so
// that backend outages cannot stall the ingest pipeline. The breaker
// trips after 5 consecutive failures and recovers via a 30s open window.
type Client struct {
	conn    driver.Conn
	opts    Options
	breaker *gobreaker.CircuitBreaker
}

// NewClient opens a pooled connection to ClickHouse and verifies it with a
// Ping. It returns an error only if the *initial* connection cannot be
// established; transient failures during normal operation are handled by
// the circuit breaker, not by returning errors at construction time.
func NewClient(opts Options) (*Client, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{opts.Addr},
		Auth: clickhouse.Auth{
			Database: opts.Database,
			Username: opts.Username,
			Password: opts.Password,
		},
		DialTimeout:     opts.DialTimeout,
		MaxOpenConns:    opts.MaxOpenConns,
		MaxIdleConns:    opts.MaxOpenConns / 2,
		ConnMaxLifetime: 30 * time.Minute,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), opts.DialTimeout)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping: %w", err)
	}

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "clickhouse-write",
		MaxRequests: 1,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.ConsecutiveFailures >= 5
		},
	})

	return &Client{conn: conn, opts: opts, breaker: cb}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// WriteBatch routes a batch of mixed-type signals to the per-table
// inserters and returns the first error encountered. Errors are wrapped
// in the circuit breaker so a struggling ClickHouse fails fast.
func (c *Client) WriteBatch(ctx context.Context, signals []signal.Signal) error {
	if len(signals) == 0 {
		return nil
	}

	metrics, logs, traces := splitByType(signals)

	_, err := c.breaker.Execute(func() (interface{}, error) {
		if len(metrics) > 0 {
			if err := c.writeMetrics(ctx, metrics); err != nil {
				return nil, err
			}
		}
		if len(logs) > 0 {
			if err := c.writeLogs(ctx, logs); err != nil {
				return nil, err
			}
		}
		if len(traces) > 0 {
			if err := c.writeTraces(ctx, traces); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// RunMigrations executes the embedded DDL one statement at a time. It is
// idempotent (the schema uses CREATE TABLE IF NOT EXISTS).
//
// Statements that fail with ClickHouse error 243 (NO_AVAILABLE_DISK) are
// SKIPPED rather than fatal: the Phase C-3b cold-tier migration sets a
// storage policy that doesn't exist on single-disk dev clusters, and
// degrading to "no cold tier" is the right behaviour there. On a
// production cluster with `storage_policies.xml` mounted, the same
// statement succeeds and the lifecycle takes effect.
func (c *Client) RunMigrations(ctx context.Context, sqlText string) error {
	for _, stmt := range splitSQLStatements(sqlText) {
		if stmt == "" {
			continue
		}
		if err := c.conn.Exec(ctx, stmt); err != nil {
			if isMissingDiskErr(err) {
				// Cold-tier disk not configured on this cluster; skip.
				continue
			}
			return fmt.Errorf("clickhouse: exec migration: %w\n--- sql ---\n%s", err, stmt)
		}
	}
	return nil
}

// isMissingDiskErr reports whether err looks like a ClickHouse 243
// (NO_AVAILABLE_DISK) failure. The driver doesn't currently expose
// a structured code for every error path, so we string-match on the
// canonical fragments. This is the same compromise the rest of the
// codebase makes for unique-constraint detection in pgx.
func isMissingDiskErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NO_AVAILABLE_DISK") ||
		strings.Contains(msg, "Code: 243") ||
		strings.Contains(msg, "is not in the list of disks") ||
		strings.Contains(msg, "Cannot find storage policy")
}

// Query is a thin pass-through used by Phase B's query engine for
// federated planning. We deliberately keep it minimal in Phase A.
func (c *Client) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("clickhouse: client not initialized")
	}
	rows, err := c.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols := rows.Columns()
	out := make([]map[string]any, 0)
	for rows.Next() {
		scan := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = scan[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ─── per-table writers ────────────────────────────────────────────────────

func (c *Client) writeMetrics(ctx context.Context, signals []signal.Signal) error {
	batch, err := c.conn.PrepareBatch(ctx,
		"INSERT INTO metrics (tenant_id, metric_name, timestamp, value, labels)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare metrics: %w", err)
	}

	for _, sig := range signals {
		name, value := parseMetricPayload(sig)
		ts := sig.ReceivedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if err := batch.Append(sig.TenantID, name, ts, value, sig.Attributes); err != nil {
			return fmt.Errorf("clickhouse: append metric: %w", err)
		}
	}
	return batch.Send()
}

func (c *Client) writeLogs(ctx context.Context, signals []signal.Signal) error {
	batch, err := c.conn.PrepareBatch(ctx,
		"INSERT INTO logs (tenant_id, service_name, severity, body, attributes, timestamp, trace_id, span_id)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare logs: %w", err)
	}

	for _, sig := range signals {
		ts := sig.ReceivedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if err := batch.Append(
			sig.TenantID,
			sig.Attributes["service_name"],
			sig.Attributes["severity"],
			string(sig.Payload),
			sig.Attributes,
			ts,
			sig.Attributes["trace_id"],
			sig.Attributes["span_id"],
		); err != nil {
			return fmt.Errorf("clickhouse: append log: %w", err)
		}
	}
	return batch.Send()
}

func (c *Client) writeTraces(ctx context.Context, signals []signal.Signal) error {
	batch, err := c.conn.PrepareBatch(ctx,
		"INSERT INTO traces (tenant_id, trace_id, span_id, parent_span_id, operation_name, service_name, start_time, end_time, duration_ns, attributes, status_code)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare traces: %w", err)
	}

	for _, sig := range signals {
		duration, _ := strconv.ParseInt(sig.Attributes["duration_ns"], 10, 64)
		start := sig.ReceivedAt
		if start.IsZero() {
			start = time.Now().UTC()
		}
		end := start.Add(time.Duration(duration))

		if err := batch.Append(
			sig.TenantID,
			sig.Attributes["trace_id"],
			sig.Attributes["span_id"],
			sig.Attributes["parent_span_id"],
			sig.Attributes["operation_name"],
			sig.Attributes["service_name"],
			start,
			end,
			duration,
			sig.Attributes,
			sig.Attributes["status_code"],
		); err != nil {
			return fmt.Errorf("clickhouse: append trace: %w", err)
		}
	}
	return batch.Send()
}

// ─── helpers ──────────────────────────────────────────────────────────────

func splitByType(in []signal.Signal) (metrics, logs, traces []signal.Signal) {
	for _, s := range in {
		switch s.Type {
		case signal.Metric:
			metrics = append(metrics, s)
		case signal.Log:
			logs = append(logs, s)
		case signal.Trace:
			traces = append(traces, s)
		}
	}
	return
}

func parseMetricPayload(sig signal.Signal) (name string, value float64) {
	name = sig.Attributes["metric_name"]
	if name == "" {
		name = "unknown"
	}
	if len(sig.Payload) == 0 {
		return
	}
	var doc struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	}
	if err := json.Unmarshal(sig.Payload, &doc); err == nil {
		if doc.Name != "" {
			name = doc.Name
		}
		value = doc.Value
	}
	return
}

// splitSQLStatements is a deliberately simple split-on-`;` parser. It
// strips line comments (`-- ...`) and blank lines and emits one
// statement per terminator. We avoid pulling in a full SQL parser
// because the migrations we own are simple by design.
func splitSQLStatements(sqlText string) []string {
	var (
		stmts   []string
		builder strings.Builder
	)
	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(trimmed)
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSuffix(strings.TrimSpace(builder.String()), ";")
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			builder.Reset()
		}
	}
	if tail := strings.TrimSpace(builder.String()); tail != "" {
		stmts = append(stmts, tail)
	}
	return stmts
}
