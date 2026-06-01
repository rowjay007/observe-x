// Logs live-tail SSE — Phase E-2.
//
// GET /v1/logs/stream?tenant_id=X[&service=Y][&severity=Z]
//
// Why polling, not a push channel from the ingest path:
//
//	The ingest-gateway and the query-engine are independent
//	services; routing per-row events from ingest into a long-lived
//	SSE channel on query-engine would require either (a) a shared
//	in-memory bus (forces the two services into a single pod), or
//	(b) Kafka/NATS as a hot side-car for every log line (operational
//	bloat for a feature that only the operator console consumes).
//
//	A 1-second poll of `SELECT … WHERE timestamp > last_seen` from
//	the same ClickHouse the operator already pays for adds <1ms of
//	query time per tick (the `(tenant_id, service_name, timestamp)`
//	ORDER BY key lets ClickHouse prune to a single granule per
//	tenant) and reuses the existing auth, RBAC, multi-tenant
//	isolation, and rate-limit paths. We can swap to a push bus
//	later under the same SSE wire shape if the workload demands.
//
// Wire frames (sse_test.go in alert-manager has the reference impl):
//
//	:keepalive\n\n                       (heartbeat every 15s)
//	event: log\ndata: {…json…}\n\n
//	event: error\ndata: {"msg":"…"}\n\n  (terminal)
//
// Auth: requires the `query` scope (same as POST /v1/query).
//
// Filters: any caller-supplied service / severity becomes an extra
// WHERE clause; injection is impossible because the values are
// passed as bound parameters to ClickHouse, not concatenated.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
)

const (
	logsTailInterval  = 1 * time.Second
	logsTailHeartbeat = 15 * time.Second
	logsTailBatchCap  = 200 // per tick; protects slow clients
)

func logsStreamHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			tenantID = c.Query("tenant_id")
		}
		if tenantID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id required"})
			return
		}
		service := c.Query("service")
		severity := c.Query("severity")

		w := c.Writer
		flusher, ok := w.(http.Flusher)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := c.Request.Context()
		// Start at "now-5s" so a freshly connected client sees a few
		// recent lines for context instead of waiting for the first
		// new line.
		lastSeen := time.Now().Add(-5 * time.Second).UTC()

		writeSSE := func(event, payload string) bool {
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		pollTicker := time.NewTicker(logsTailInterval)
		defer pollTicker.Stop()
		hbTicker := time.NewTicker(logsTailHeartbeat)
		defer hbTicker.Stop()

		// Build the SQL once. We bind tenant_id + optional filters via
		// `?` so values can never be SQL-escaped wrong.
		sql := "SELECT timestamp, severity, service_name, body, trace_id, span_id, attributes " +
			"FROM logs WHERE tenant_id = ? AND timestamp > ?"
		args := []any{tenantID, lastSeen}
		if service != "" {
			sql += " AND service_name = ?"
			args = append(args, service)
		}
		if severity != "" {
			sql += " AND severity = ?"
			args = append(args, severity)
		}
		sql += " ORDER BY timestamp ASC LIMIT ?"
		args = append(args, logsTailBatchCap)
		// args[1] is the cursor; we mutate it each tick.

		for {
			select {
			case <-ctx.Done():
				return
			case <-hbTicker.C:
				if !writeSSE("heartbeat", `{"t":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`) {
					return
				}
			case <-pollTicker.C:
				args[1] = lastSeen
				rows, err := client.Query(ctx, sql, args...)
				if err != nil {
					logger.Warn("logs tail query", zap.Error(err), zap.String("tenant", tenantID))
					_ = writeSSE("error", `{"msg":"upstream"}`)
					return
				}
				for _, row := range rows {
					// Advance the cursor strictly past the row we
					// just emitted so duplicates are impossible even
					// at sub-microsecond resolution.
					if ts, ok := timestampOf(row["timestamp"]); ok && ts.After(lastSeen) {
						lastSeen = ts
					}
					b, err := json.Marshal(row)
					if err != nil {
						continue
					}
					if !writeSSE("log", string(b)) {
						return
					}
				}
			}
		}
	}
}

func timestampOf(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	case string:
		if t == "" {
			return time.Time{}, false
		}
		// ClickHouse driver typically hands us time.Time directly,
		// but RFC 3339 strings are a sane fallback.
		if pt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return pt, true
		}
		if pt, err := time.Parse(time.RFC3339, t); err == nil {
			return pt, true
		}
	}
	return time.Time{}, false
}
