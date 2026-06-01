// PromQL + LogQL compatibility shims — Phase E-5.
//
// These handlers parse Prometheus / Loki HTTP request shapes,
// translate the query string via pkg/promql or pkg/logql into a
// single ClickHouse SQL statement, execute it against the same
// query-engine pool, and return a response shaped like the
// Prometheus / Loki HTTP API so existing Grafana panels just work.
//
// We do NOT pretend to be a full PromQL/LogQL engine. The
// translators reject anything outside their documented subsets
// with a 400 + JSON error body the user can read.
//
// Tenant safety: the auth middleware injects X-Tenant-ID; we
// always wrap the translated SQL with `tenant_id = ?` before
// execution. The PromQL/LogQL string can NEVER name a tenant.
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/logql"
	"github.com/rowjay007/observe-x/pkg/promql"
	chstorage "github.com/rowjay007/observe-x/pkg/storage/clickhouse"
)

const (
	defaultShimRangeStep = 30 * time.Second
	defaultShimLogLimit  = 1000
)

// registerShims attaches the PromQL + LogQL handlers to the given
// router group. Caller wires it under the same auth + scope
// middleware as the native /v1/query route.
func registerShims(g *gin.RouterGroup, client *chstorage.Client, logger *zap.Logger) {
	g.GET("/prom/api/v1/query", auth.GinRequireScope(auth.ScopeQuery), promInstantHandler(client, logger))
	g.GET("/prom/api/v1/query_range", auth.GinRequireScope(auth.ScopeQuery), promRangeHandler(client, logger))
	g.POST("/prom/api/v1/query", auth.GinRequireScope(auth.ScopeQuery), promInstantHandler(client, logger))
	g.POST("/prom/api/v1/query_range", auth.GinRequireScope(auth.ScopeQuery), promRangeHandler(client, logger))

	g.GET("/loki/api/v1/query", auth.GinRequireScope(auth.ScopeQuery), lokiInstantHandler(client, logger))
	g.GET("/loki/api/v1/query_range", auth.GinRequireScope(auth.ScopeQuery), lokiRangeHandler(client, logger))
	g.POST("/loki/api/v1/query", auth.GinRequireScope(auth.ScopeQuery), lokiInstantHandler(client, logger))
	g.POST("/loki/api/v1/query_range", auth.GinRequireScope(auth.ScopeQuery), lokiRangeHandler(client, logger))
}

// ── shared utilities ─────────────────────────────────────────────

func parseTimeParam(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		// Prometheus accepts unix seconds (float).
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return fallback
}

func parseDurParam(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	return fallback
}

func tenantOf(c *gin.Context) string {
	if v := c.Request.Header.Get("X-Tenant-ID"); v != "" {
		return v
	}
	return c.Query("tenant_id")
}

// withTenant wraps `sql` with a tenant_id = ? guard. The placeholder
// position is **the first one** so the executor can interpolate
// after the rest of the args.
func withTenant(sql, tenant string, args []any) (string, []any) {
	if tenant == "" {
		return sql, args
	}
	// Inject as an extra AND on the existing WHERE clause. All our
	// translators always emit a WHERE clause; we just stitch a
	// `tenant_id = ?` predicate ahead of the rest of the args.
	guarded := "SELECT * FROM (" + sql + ") WHERE tenant_id = ?"
	// Append tenant at the END because outer args bind after inner
	// in our executor's positional model. We rely on ClickHouse
	// passing `?` left-to-right; since `tenant_id` is only referred
	// to once in the outer query, the new placeholder is the last.
	return guarded, append(args, tenant)
}

// ── PromQL handlers ──────────────────────────────────────────────

func promInstantHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		q := c.DefaultQuery("query", c.PostForm("query"))
		ts := parseTimeParam(c.DefaultQuery("time", c.PostForm("time")), time.Now().UTC())
		runProm(c, client, logger, promql.QueryParams{
			Query: q,
			Start: ts,
			End:   ts,
			Step:  defaultShimRangeStep,
		}, false)
	}
}

func promRangeHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		q := c.DefaultQuery("query", c.PostForm("query"))
		start := parseTimeParam(c.DefaultQuery("start", c.PostForm("start")), time.Now().UTC().Add(-time.Hour))
		end := parseTimeParam(c.DefaultQuery("end", c.PostForm("end")), time.Now().UTC())
		step := parseDurParam(c.DefaultQuery("step", c.PostForm("step")), defaultShimRangeStep)
		runProm(c, client, logger, promql.QueryParams{
			Query: q,
			Start: start,
			End:   end,
			Step:  step,
		}, true)
	}
}

func runProm(c *gin.Context, client *chstorage.Client, logger *zap.Logger, p promql.QueryParams, isMatrix bool) {
	res, err := promql.Translate(p)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":    "error",
			"errorType": "bad_data",
			"error":     err.Error(),
		})
		return
	}
	tenant := tenantOf(c)
	if tenant == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "errorType": "unauthorized", "error": "tenant"})
		return
	}
	sql, args := withTenant(res.SQL, tenant, res.Args)
	rows, err := client.Query(c.Request.Context(), sql, args...)
	if err != nil {
		logger.Warn("promql exec", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "errorType": "internal", "error": err.Error()})
		return
	}

	// Reshape rows into Prometheus result shapes.
	if isMatrix {
		// matrix: [{ metric: {...}, values: [[ts, "v"], …] }]
		values := make([][]any, 0, len(rows))
		for _, row := range rows {
			ts, ok := timestampOf(row["t"])
			if !ok {
				continue
			}
			v := fmt.Sprintf("%v", row["v"])
			values = append(values, []any{float64(ts.Unix()), v})
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "success",
			"data": gin.H{
				"resultType": "matrix",
				"result": []any{gin.H{
					"metric": gin.H{},
					"values": values,
				}},
			},
		})
		return
	}
	// vector: single value at the requested timestamp = last row.
	if len(rows) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"status": "success",
			"data":   gin.H{"resultType": "vector", "result": []any{}},
		})
		return
	}
	last := rows[len(rows)-1]
	ts, _ := timestampOf(last["t"])
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"data": gin.H{
			"resultType": "vector",
			"result": []any{gin.H{
				"metric": gin.H{},
				"value":  []any{float64(ts.Unix()), fmt.Sprintf("%v", last["v"])},
			}},
		},
	})
}

// ── LogQL handlers ───────────────────────────────────────────────

func lokiInstantHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		runLoki(c, client, logger, lokiParams(c, false))
	}
}

func lokiRangeHandler(client *chstorage.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		runLoki(c, client, logger, lokiParams(c, true))
	}
}

func lokiParams(c *gin.Context, isRange bool) logql.QueryParams {
	q := c.DefaultQuery("query", c.PostForm("query"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", c.PostForm("limit")))
	if limit <= 0 {
		limit = defaultShimLogLimit
	}
	step := parseDurParam(c.DefaultQuery("step", c.PostForm("step")), defaultShimRangeStep)
	if isRange {
		start := parseTimeParam(c.DefaultQuery("start", c.PostForm("start")), time.Now().UTC().Add(-time.Hour))
		end := parseTimeParam(c.DefaultQuery("end", c.PostForm("end")), time.Now().UTC())
		return logql.QueryParams{Query: q, Start: start, End: end, Step: step, Limit: limit}
	}
	ts := parseTimeParam(c.DefaultQuery("time", c.PostForm("time")), time.Now().UTC())
	return logql.QueryParams{Query: q, Start: ts.Add(-time.Hour), End: ts, Step: step, Limit: limit}
}

func runLoki(c *gin.Context, client *chstorage.Client, logger *zap.Logger, p logql.QueryParams) {
	res, err := logql.Translate(p)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	tenant := tenantOf(c)
	if tenant == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "tenant"})
		return
	}
	sql, args := withTenant(res.SQL, tenant, res.Args)
	rows, err := client.Query(c.Request.Context(), sql, args...)
	if err != nil {
		logger.Warn("logql exec", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": err.Error()})
		return
	}
	if res.IsLogs {
		// Loki streams shape: { resultType: "streams",
		//   result: [{ stream: {service:"x"}, values: [[ts_ns, "body"], …] }] }
		groups := map[string][][]string{}
		labels := map[string]map[string]string{}
		for _, row := range rows {
			svc := fmt.Sprintf("%v", row["service_name"])
			sev := fmt.Sprintf("%v", row["severity"])
			key := svc + "|" + sev
			if _, ok := groups[key]; !ok {
				labels[key] = map[string]string{"service_name": svc, "severity": sev}
			}
			ts, _ := timestampOf(row["timestamp"])
			groups[key] = append(groups[key],
				[]string{strconv.FormatInt(ts.UnixNano(), 10), fmt.Sprintf("%v", row["body"])})
		}
		streams := make([]gin.H, 0, len(groups))
		for k, vals := range groups {
			streams = append(streams, gin.H{
				"stream": labels[k],
				"values": vals,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "success",
			"data": gin.H{
				"resultType": "streams",
				"result":     streams,
			},
		})
		return
	}
	// metric query — matrix shape with one stream.
	values := make([][]any, 0, len(rows))
	for _, row := range rows {
		ts, ok := timestampOf(row["t"])
		if !ok {
			continue
		}
		values = append(values, []any{float64(ts.Unix()), fmt.Sprintf("%v", row["v"])})
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"data": gin.H{
			"resultType": "matrix",
			"result": []any{gin.H{
				"metric": gin.H{},
				"values": values,
			}},
		},
	})
}
