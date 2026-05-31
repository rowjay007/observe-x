// Package otlp implements OTLP/HTTP receivers. Phase B-2 replaces the
// Phase A byte-passthrough with real protobuf decoding for traces,
// metrics, and logs, mapping each into the internal Signal type.
//
// Endpoints follow the OTLP/HTTP spec:
//
//	POST /v1/traces      application/x-protobuf  (gzip optional)
//	POST /v1/metrics     application/x-protobuf  (gzip optional)
//	POST /v1/logs        application/x-protobuf  (gzip optional)
//
// Response codes follow the spec too:
//
//	200 — accepted in full (we return an empty ExportXResponse)
//	400 — malformed protobuf or unsupported content-type
//	413 — body too large
//	429 — back-pressure (engine queue full)
//	503 — internal accept failed (e.g. ClickHouse circuit open)
//
// See docs/adr/0005-otlp-adoption.md.
package otlp

import (
	"compress/gzip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/rowjay007/observe-x/pkg/signal"
)

// MaxRequestBytes caps an OTLP body. Beyond this we reject with 413
// before allocating the decoder. 8 MiB matches the OTLP Collector default.
const MaxRequestBytes = 8 << 20

// ErrBodyTooLarge is returned when the request body exceeds MaxRequestBytes.
var ErrBodyTooLarge = errors.New("otlp: request body exceeds limit")

// ─── Body reader (gzip-aware, capped) ─────────────────────────────────────

// ReadBody reads an OTLP/HTTP request body, transparently handling
// gzip, and refuses anything larger than MaxRequestBytes.
func ReadBody(r io.Reader, contentEncoding string) ([]byte, error) {
	limited := io.LimitReader(r, MaxRequestBytes+1)

	var src io.Reader = limited
	if contentEncoding == "gzip" {
		zr, err := gzip.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("otlp: gzip: %w", err)
		}
		defer func() { _ = zr.Close() }()
		src = zr
	}

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRequestBytes {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

// ─── Trace decoder ────────────────────────────────────────────────────────

// DecodeTraces parses an ExportTraceServiceRequest payload and returns
// one Signal per Span. Resource and scope attributes are flattened into
// each Signal's Attributes so downstream stages don't need OTLP-specific
// awareness.
func DecodeTraces(tenantID string, body []byte, receivedAt time.Time) ([]signal.Signal, error) {
	var req tracepb.TracesData
	if err := proto.Unmarshal(body, &req); err != nil {
		// Try the ExportTraceServiceRequest wire shape too — both are
		// permitted by collectors and SDKs in the wild.
		var alt tracepb.ResourceSpans
		if err2 := proto.Unmarshal(body, &alt); err2 != nil {
			return nil, fmt.Errorf("otlp: decode traces: %w", err)
		}
		req.ResourceSpans = []*tracepb.ResourceSpans{&alt}
	}

	out := make([]signal.Signal, 0, 16)
	for _, rs := range req.GetResourceSpans() {
		resourceAttrs := attrsToMap(rs.GetResource())
		for _, ss := range rs.GetScopeSpans() {
			scopeName := ss.GetScope().GetName()
			for _, span := range ss.GetSpans() {
				attrs := mergeAttrs(resourceAttrs, kvToMap(span.GetAttributes()))
				if scopeName != "" {
					attrs["otel.scope.name"] = scopeName
				}
				attrs["trace_id"] = hex.EncodeToString(span.GetTraceId())
				attrs["span_id"] = hex.EncodeToString(span.GetSpanId())
				if parent := span.GetParentSpanId(); len(parent) > 0 {
					attrs["parent_span_id"] = hex.EncodeToString(parent)
				}
				attrs["span.name"] = span.GetName()
				attrs["span.kind"] = span.GetKind().String()

				start := span.GetStartTimeUnixNano()
				end := span.GetEndTimeUnixNano()
				if end > start {
					attrs["duration_ms"] = strconv.FormatFloat(
						float64(end-start)/1e6, 'f', 3, 64)
				}

				if status := span.GetStatus(); status != nil {
					if status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
						attrs["severity"] = "ERROR"
					}
					if msg := status.GetMessage(); msg != "" {
						attrs["status.message"] = msg
					}
				}

				out = append(out, signal.Signal{
					TenantID:   tenantID,
					Type:       signal.Trace,
					Payload:    nil, // structured attributes carry the data
					Attributes: attrs,
					ReceivedAt: receivedAt,
				})
			}
		}
	}
	return out, nil
}

// ─── Metric decoder ───────────────────────────────────────────────────────

// DecodeMetrics parses a MetricsData payload. Each data point becomes
// one Signal — for Sum/Gauge, the value lives in attributes["value"];
// for Histograms, attributes["count"] and ["sum"] are populated.
// Exponential/summary histograms are accepted but reduced to count+sum.
func DecodeMetrics(tenantID string, body []byte, receivedAt time.Time) ([]signal.Signal, error) {
	var req metricspb.MetricsData
	if err := proto.Unmarshal(body, &req); err != nil {
		var alt metricspb.ResourceMetrics
		if err2 := proto.Unmarshal(body, &alt); err2 != nil {
			return nil, fmt.Errorf("otlp: decode metrics: %w", err)
		}
		req.ResourceMetrics = []*metricspb.ResourceMetrics{&alt}
	}

	out := make([]signal.Signal, 0, 16)
	for _, rm := range req.GetResourceMetrics() {
		resourceAttrs := attrsToMap(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			scopeName := sm.GetScope().GetName()
			for _, m := range sm.GetMetrics() {
				baseAttrs := mergeAttrs(resourceAttrs, nil)
				if scopeName != "" {
					baseAttrs["otel.scope.name"] = scopeName
				}
				baseAttrs["metric_name"] = m.GetName()
				if u := m.GetUnit(); u != "" {
					baseAttrs["metric_unit"] = u
				}
				if d := m.GetDescription(); d != "" {
					baseAttrs["metric_description"] = d
				}

				switch data := m.Data.(type) {
				case *metricspb.Metric_Gauge:
					for _, dp := range data.Gauge.GetDataPoints() {
						attrs := mergeAttrs(baseAttrs, kvToMap(dp.GetAttributes()))
						attrs["value"] = numberDPValue(dp)
						out = append(out, makeMetric(tenantID, attrs, dp.GetTimeUnixNano(), receivedAt))
					}
				case *metricspb.Metric_Sum:
					for _, dp := range data.Sum.GetDataPoints() {
						attrs := mergeAttrs(baseAttrs, kvToMap(dp.GetAttributes()))
						attrs["value"] = numberDPValue(dp)
						attrs["temporality"] = data.Sum.GetAggregationTemporality().String()
						out = append(out, makeMetric(tenantID, attrs, dp.GetTimeUnixNano(), receivedAt))
					}
				case *metricspb.Metric_Histogram:
					for _, dp := range data.Histogram.GetDataPoints() {
						attrs := mergeAttrs(baseAttrs, kvToMap(dp.GetAttributes()))
						attrs["count"] = strconv.FormatUint(dp.GetCount(), 10)
						attrs["sum"] = strconv.FormatFloat(dp.GetSum(), 'f', -1, 64)
						out = append(out, makeMetric(tenantID, attrs, dp.GetTimeUnixNano(), receivedAt))
					}
				case *metricspb.Metric_ExponentialHistogram:
					for _, dp := range data.ExponentialHistogram.GetDataPoints() {
						attrs := mergeAttrs(baseAttrs, kvToMap(dp.GetAttributes()))
						attrs["count"] = strconv.FormatUint(dp.GetCount(), 10)
						attrs["sum"] = strconv.FormatFloat(dp.GetSum(), 'f', -1, 64)
						out = append(out, makeMetric(tenantID, attrs, dp.GetTimeUnixNano(), receivedAt))
					}
				case *metricspb.Metric_Summary:
					for _, dp := range data.Summary.GetDataPoints() {
						attrs := mergeAttrs(baseAttrs, kvToMap(dp.GetAttributes()))
						attrs["count"] = strconv.FormatUint(dp.GetCount(), 10)
						attrs["sum"] = strconv.FormatFloat(dp.GetSum(), 'f', -1, 64)
						out = append(out, makeMetric(tenantID, attrs, dp.GetTimeUnixNano(), receivedAt))
					}
				}
			}
		}
	}
	return out, nil
}

func numberDPValue(dp *metricspb.NumberDataPoint) string {
	switch v := dp.Value.(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return strconv.FormatFloat(v.AsDouble, 'f', -1, 64)
	case *metricspb.NumberDataPoint_AsInt:
		return strconv.FormatInt(v.AsInt, 10)
	}
	return ""
}

func makeMetric(tenant string, attrs map[string]string, tsNS uint64, fallback time.Time) signal.Signal {
	ts := fallback
	if tsNS > 0 {
		ts = time.Unix(0, int64(tsNS)).UTC()
	}
	return signal.Signal{
		TenantID:   tenant,
		Type:       signal.Metric,
		Attributes: attrs,
		ReceivedAt: ts,
	}
}

// ─── Log decoder ──────────────────────────────────────────────────────────

func DecodeLogs(tenantID string, body []byte, receivedAt time.Time) ([]signal.Signal, error) {
	var req logspb.LogsData
	if err := proto.Unmarshal(body, &req); err != nil {
		var alt logspb.ResourceLogs
		if err2 := proto.Unmarshal(body, &alt); err2 != nil {
			return nil, fmt.Errorf("otlp: decode logs: %w", err)
		}
		req.ResourceLogs = []*logspb.ResourceLogs{&alt}
	}

	out := make([]signal.Signal, 0, 16)
	for _, rl := range req.GetResourceLogs() {
		resourceAttrs := attrsToMap(rl.GetResource())
		for _, sl := range rl.GetScopeLogs() {
			scopeName := sl.GetScope().GetName()
			for _, rec := range sl.GetLogRecords() {
				attrs := mergeAttrs(resourceAttrs, kvToMap(rec.GetAttributes()))
				if scopeName != "" {
					attrs["otel.scope.name"] = scopeName
				}
				if sev := rec.GetSeverityText(); sev != "" {
					attrs["severity"] = sev
				} else if num := rec.GetSeverityNumber(); num >= logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
					attrs["severity"] = "ERROR"
				}
				if traceID := rec.GetTraceId(); len(traceID) > 0 {
					attrs["trace_id"] = hex.EncodeToString(traceID)
				}
				if spanID := rec.GetSpanId(); len(spanID) > 0 {
					attrs["span_id"] = hex.EncodeToString(spanID)
				}
				body := anyValueString(rec.GetBody())

				ts := receivedAt
				if t := rec.GetTimeUnixNano(); t > 0 {
					ts = time.Unix(0, int64(t)).UTC()
				}

				out = append(out, signal.Signal{
					TenantID:   tenantID,
					Type:       signal.Log,
					Payload:    []byte(body),
					Attributes: attrs,
					ReceivedAt: ts,
				})
			}
		}
	}
	return out, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────

func attrsToMap(r *resourcepb.Resource) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return kvToMap(r.GetAttributes())
}

func kvToMap(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if kv == nil || kv.Key == "" {
			continue
		}
		out[kv.Key] = anyValueString(kv.Value)
	}
	return out
}

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(val.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]byte, 0, 32)
		parts = append(parts, '[')
		for i, item := range val.ArrayValue.GetValues() {
			if i > 0 {
				parts = append(parts, ',')
			}
			parts = append(parts, []byte(anyValueString(item))...)
		}
		parts = append(parts, ']')
		return string(parts)
	}
	return ""
}

func mergeAttrs(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}
