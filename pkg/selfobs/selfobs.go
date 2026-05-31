// Package selfobs is ObserveX's self-observability glue. It wraps the
// OpenTelemetry SDK with our conventions — service.name, sampling
// fraction, OTLP/HTTP exporter pointed at the ingest-gateway —
// keeping the per-service main.go free of boilerplate.
//
// Design intent:
//
//   * Each service emits its own traces *back into ObserveX*. The
//     ingest-gateway's OTLP receiver (Phase B-2) accepts them, the
//     storage backend persists them, the query-engine surfaces them.
//     This closes the dogfood loop: we instrument our platform with
//     our platform.
//
//   * The default sampling fraction is 0.10 — high enough to catch
//     production issues, low enough to keep self-instrumentation
//     overhead under 5% in the steady state.
//
//   * If OBSERVE_X_OTLP_ENDPOINT is unset, Init returns a no-op
//     TracerProvider. We never crash a service for missing
//     observability config.
package selfobs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config carries the per-service tuning knobs. Required fields are
// only ServiceName; everything else has sensible defaults that the
// env-driven InitFromEnv constructor wires up.
type Config struct {
	ServiceName       string
	ServiceVersion    string
	Endpoint          string  // e.g. "http://ingest-gateway:7000"
	APIKey            string  // tenant API key used as Bearer token
	TenantID          string  // X-Tenant-ID header value
	SamplingFraction  float64 // [0,1]; 0 → never sample, 1 → always
	ExportTimeout     time.Duration
	BatchSize         int
	Insecure          bool // skip TLS, http:// rather than https://
}

func (c Config) withDefaults() Config {
	if c.ServiceVersion == "" {
		c.ServiceVersion = "1.0.0"
	}
	if c.SamplingFraction == 0 {
		c.SamplingFraction = 0.10
	}
	if c.ExportTimeout == 0 {
		c.ExportTimeout = 5 * time.Second
	}
	if c.BatchSize == 0 {
		c.BatchSize = 512
	}
	return c
}

// Provider is the handle returned by Init. Call Shutdown at process
// exit (or via t.Cleanup in tests) to flush in-flight spans.
type Provider struct {
	tp       trace.TracerProvider
	shutdown func(context.Context) error
}

func (p *Provider) Tracer(name string) trace.Tracer {
	return p.tp.Tracer(name)
}

func (p *Provider) Shutdown(ctx context.Context) error {
	if p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Init wires up the OTLP/HTTP exporter, batch span processor, and
// global propagator. Returns a no-op provider when cfg.Endpoint is
// empty — explicitly: missing observability config is NOT a startup
// failure.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.ServiceName == "" {
		return nil, fmt.Errorf("selfobs: ServiceName required")
	}
	cfg = cfg.withDefaults()

	if cfg.Endpoint == "" {
		// No exporter ⇒ no-op provider; never fail startup for missing
		// observability config.
		ntp := noop.NewTracerProvider()
		otel.SetTracerProvider(ntp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{}))
		return &Provider{tp: ntp}, nil
	}

	endpoint := stripScheme(cfg.Endpoint)

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithTimeout(cfg.ExportTimeout),
		otlptracehttp.WithURLPath("/v1/traces"),
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}

	headers := map[string]string{}
	if cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.APIKey
	}
	if cfg.TenantID != "" {
		headers["X-Tenant-ID"] = cfg.TenantID
	}
	if len(headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracehttp.WithHeaders(headers))
	}

	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("selfobs: exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			attribute.String("observex.component", "platform"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("selfobs: resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(cfg.BatchSize),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SamplingFraction))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return &Provider{
		tp:       tp,
		shutdown: tp.Shutdown,
	}, nil
}

// InitFromEnv reads the standard ObserveX env vars:
//
//   OBSERVE_X_OTLP_ENDPOINT          base URL of the ingest-gateway
//   OBSERVE_X_OTLP_API_KEY           tenant key for the loopback
//   OBSERVE_X_OTLP_TENANT_ID         tenant ID for the loopback
//   OBSERVE_X_OTLP_SAMPLING          0..1, default 0.10
//   OBSERVE_X_OTLP_INSECURE          "1" to skip TLS (dev only)
//
// serviceName/serviceVersion come from the caller (each service
// passes its own ldflags-injected values).
func InitFromEnv(ctx context.Context, serviceName, serviceVersion string) (*Provider, error) {
	cfg := Config{
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		Endpoint:       os.Getenv("OBSERVE_X_OTLP_ENDPOINT"),
		APIKey:         os.Getenv("OBSERVE_X_OTLP_API_KEY"),
		TenantID:       os.Getenv("OBSERVE_X_OTLP_TENANT_ID"),
		Insecure:       os.Getenv("OBSERVE_X_OTLP_INSECURE") == "1",
	}
	if s := os.Getenv("OBSERVE_X_OTLP_SAMPLING"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f <= 1 {
			cfg.SamplingFraction = f
		}
	}
	return Init(ctx, cfg)
}

func stripScheme(s string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return s[len(prefix):]
		}
	}
	return s
}
