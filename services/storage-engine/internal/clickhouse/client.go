package clickhouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/rowjay007/observe-x/pkg/signal"
	"sync"
)

type Client struct {
	mu        sync.Mutex
	db        *sql.DB
	addr      string
	batchSize int
	batch     []*signal.Signal
}

func NewClient(addr string, batchSize int) (*Client, error) {
	dsn := fmt.Sprintf("tcp://%s", addr)
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open clickhouse connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping clickhouse: %w", err)
	}

	return &Client{
		db:        db,
		addr:      addr,
		batchSize: batchSize,
		batch:     make([]*signal.Signal, 0, batchSize),
	}, nil
}

func (c *Client) Write(ctx context.Context, signals []signal.Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range signals {
		c.batch = append(c.batch, &signals[i])

		if len(c.batch) >= c.batchSize {
			if err := c.flushBatch(ctx); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Client) flushBatch(ctx context.Context) error {
	if len(c.batch) == 0 {
		return nil
	}

	switch c.batch[0].Type {
	case signal.Metric:
		if err := c.writeMetrics(ctx, c.batch); err != nil {
			return err
		}
	case signal.Log:
		if err := c.writeLogs(ctx, c.batch); err != nil {
			return err
		}
	case signal.Trace:
		if err := c.writeTraces(ctx, c.batch); err != nil {
			return err
		}
	}

	c.batch = c.batch[:0]
	return nil
}

func (c *Client) writeMetrics(ctx context.Context, signals []*signal.Signal) error {
	query := `
	INSERT INTO metrics (tenant_id, metric_name, timestamp, value, labels)
	VALUES (?, ?, now64(3, 'UTC'), ?, ?)
	`

	stmt, err := c.db.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to prepare metrics insert: %w", err)
	}
	defer stmt.Close()

	for _, sig := range signals {
		metricData := parseMetricPayload(sig)
		if _, err := stmt.ExecContext(ctx, sig.TenantID, metricData.Name, metricData.Value, sig.Attributes); err != nil {
			return fmt.Errorf("failed to insert metric: %w", err)
		}
	}

	return nil
}

type metricData struct {
	Name  string
	Value float64
}

func parseMetricPayload(sig *signal.Signal) metricData {
	data := metricData{Name: "unknown"}

	if name, ok := sig.Attributes["metric_name"]; ok {
		data.Name = name
	}

	if len(sig.Payload) > 0 {
		var payload map[string]interface{}
		if err := json.Unmarshal(sig.Payload, &payload); err == nil {
			if v, ok := payload["value"].(float64); ok {
				data.Value = v
			}
		}
	}

	return data
}

func (c *Client) writeLogs(ctx context.Context, signals []*signal.Signal) error {
	query := `
	INSERT INTO logs (tenant_id, service_name, severity, body, attributes, timestamp, trace_id, span_id)
	VALUES (?, ?, ?, ?, ?, now64(3, 'UTC'), ?, ?)
	`

	stmt, err := c.db.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to prepare logs insert: %w", err)
	}
	defer stmt.Close()

	for _, sig := range signals {
		logData := parseLogPayload(sig)
		if _, err := stmt.ExecContext(ctx, sig.TenantID, logData.ServiceName, logData.Severity, logData.Body, sig.Attributes, logData.TraceID, logData.SpanID); err != nil {
			return fmt.Errorf("failed to insert log: %w", err)
		}
	}

	return nil
}

type logData struct {
	ServiceName string
	Severity    string
	Body        string
	TraceID     string
	SpanID      string
}

func parseLogPayload(sig *signal.Signal) logData {
	return logData{
		ServiceName: sig.Attributes["service_name"],
		Severity:    sig.Attributes["severity"],
		Body:        string(sig.Payload),
		TraceID:     sig.Attributes["trace_id"],
		SpanID:      sig.Attributes["span_id"],
	}
}

func (c *Client) writeTraces(ctx context.Context, signals []*signal.Signal) error {
	query := `
	INSERT INTO traces (tenant_id, trace_id, span_id, parent_span_id, operation_name, service_name, start_time, end_time, duration_ns, attributes, status_code)
	VALUES (?, ?, ?, ?, ?, ?, now64(6, 'UTC'), now64(6, 'UTC'), ?, ?, ?)
	`

	stmt, err := c.db.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to prepare traces insert: %w", err)
	}
	defer stmt.Close()

	for _, sig := range signals {
		traceData := parseTracePayload(sig)
		if _, err := stmt.ExecContext(ctx, sig.TenantID, traceData.TraceID, traceData.SpanID, traceData.ParentSpanID, traceData.OperationName, traceData.ServiceName, traceData.DurationNs, sig.Attributes, traceData.StatusCode); err != nil {
			return fmt.Errorf("failed to insert trace: %w", err)
		}
	}

	return nil
}

type traceData struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	OperationName string
	ServiceName   string
	StatusCode    string
	DurationNs    int64
}

func parseTracePayload(sig *signal.Signal) traceData {
	data := traceData{
		TraceID:       sig.Attributes["trace_id"],
		SpanID:        sig.Attributes["span_id"],
		ParentSpanID:  sig.Attributes["parent_span_id"],
		OperationName: sig.Attributes["operation_name"],
		ServiceName:   sig.Attributes["service_name"],
		StatusCode:    sig.Attributes["status_code"],
	}

	if duration, ok := sig.Attributes["duration_ns"]; ok {
		fmt.Sscanf(duration, "%d", &data.DurationNs)
	}

	return data
}

func (c *Client) Flush(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushBatch(ctx)
}

func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}
