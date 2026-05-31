package otlp

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestDecodeTracesPreservesIDsAndAttributes(t *testing.T) {
	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := []byte{0xa, 0xb, 0xc, 0xd, 0xe, 0xf, 0, 1}

	req := &tracepb.TracesData{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				kv("service.name", "checkout"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "test"},
				Spans: []*tracepb.Span{{
					TraceId:           traceID,
					SpanId:            spanID,
					Name:              "GET /pay",
					StartTimeUnixNano: 1_000_000_000,
					EndTimeUnixNano:   1_500_000_000,
					Kind:              tracepb.Span_SPAN_KIND_SERVER,
					Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
					Attributes:        []*commonpb.KeyValue{kv("http.method", "GET")},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	sigs, err := DecodeTraces("acme", body, time.Now())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("want 1 signal, got %d", len(sigs))
	}
	s := sigs[0]
	if s.TenantID != "acme" || s.Type != signal.Trace {
		t.Fatalf("envelope wrong: %+v", s)
	}
	if got := s.Attributes["trace_id"]; got != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %q", got)
	}
	if got := s.Attributes["span_id"]; got != "0a0b0c0d0e0f0001" {
		t.Errorf("span_id = %q", got)
	}
	if s.Attributes["service.name"] != "checkout" {
		t.Error("resource attribute lost")
	}
	if s.Attributes["http.method"] != "GET" {
		t.Error("span attribute lost")
	}
	if s.Attributes["severity"] != "ERROR" {
		t.Error("status code → severity lost")
	}
	if s.Attributes["duration_ms"] != "500.000" {
		t.Errorf("duration_ms = %q", s.Attributes["duration_ms"])
	}
}

func TestDecodeMetricsSumAndHistogram(t *testing.T) {
	req := &metricspb.MetricsData{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "api")}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{
					{
						Name: "http.requests",
						Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
							AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
							DataPoints: []*metricspb.NumberDataPoint{{
								TimeUnixNano: uint64(time.Now().UnixNano()),
								Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 42},
								Attributes:   []*commonpb.KeyValue{kv("method", "GET")},
							}},
						}},
					},
					{
						Name: "request.duration",
						Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
							DataPoints: []*metricspb.HistogramDataPoint{{
								TimeUnixNano: uint64(time.Now().UnixNano()),
								Count:        10,
								Sum:          proto.Float64(1.5),
							}},
						}},
					},
				},
			}},
		}},
	}
	body, _ := proto.Marshal(req)

	sigs, err := DecodeMetrics("acme", body, time.Now())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("want 2 signals, got %d", len(sigs))
	}
	var sum, hist signal.Signal
	for _, s := range sigs {
		switch s.Attributes["metric_name"] {
		case "http.requests":
			sum = s
		case "request.duration":
			hist = s
		}
	}
	if sum.Attributes["value"] != "42" {
		t.Errorf("sum value = %q", sum.Attributes["value"])
	}
	if sum.Attributes["method"] != "GET" {
		t.Errorf("datapoint attr lost: %v", sum.Attributes)
	}
	if hist.Attributes["count"] != "10" || hist.Attributes["sum"] != "1.5" {
		t.Errorf("histogram = %+v", hist.Attributes)
	}
}

func TestDecodeLogsSeverityAndCorrelation(t *testing.T) {
	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	req := &logspb.LogsData{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "api")}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano:   uint64(time.Now().UnixNano()),
					SeverityText:   "ERROR",
					SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
					TraceId:        traceID,
					Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom"}},
				}},
			}},
		}},
	}
	body, _ := proto.Marshal(req)

	sigs, err := DecodeLogs("acme", body, time.Now())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("want 1 signal, got %d", len(sigs))
	}
	s := sigs[0]
	if string(s.Payload) != "boom" {
		t.Errorf("body lost: %q", s.Payload)
	}
	if s.Attributes["severity"] != "ERROR" {
		t.Errorf("severity = %q", s.Attributes["severity"])
	}
	if !strings.HasPrefix(s.Attributes["trace_id"], "010203") {
		t.Errorf("trace_id = %q", s.Attributes["trace_id"])
	}
}

func TestReadBodyHandlesGzipAndLimit(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, 1024)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()

	got, err := ReadBody(&buf, "gzip")
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Error("gzip round-trip mismatch")
	}

	huge := bytes.NewReader(bytes.Repeat([]byte{0}, MaxRequestBytes+10))
	if _, err := ReadBody(huge, ""); err != ErrBodyTooLarge {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
}

func TestDecodeTracesRejectsGarbage(t *testing.T) {
	if _, err := DecodeTraces("acme", []byte("nope"), time.Now()); err == nil {
		t.Error("expected error on malformed payload")
	}
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
