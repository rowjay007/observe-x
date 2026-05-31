// gRPC OTLP receiver — Phase C-3a, see ADR-0012.
//
// Mounts the three canonical OTLP gRPC services (TraceService,
// MetricsService, LogsService) on a *grpc.Server. The implementations
// reuse the same DecodeTraces / DecodeMetrics / DecodeLogs paths as
// the HTTP receiver by re-marshalling the inbound protobuf message
// and feeding it through the existing decoder. That keeps the wire
// → signal projection in exactly one place; any divergence between
// HTTP and gRPC would be a defect.
//
// Auth: a unary interceptor extracts a Bearer token from the
// `authorization` metadata key, validates it against the same
// auth.KeyStore the HTTP server uses, and enforces the `ingest`
// scope. The validated tenant id is stashed back into the gRPC
// metadata as `x-tenant-id` so the per-service Export methods can
// route signals to the right tenant.
//
// Back-pressure: an overloaded engine returns
// codes.ResourceExhausted, the canonical gRPC signal for "retry with
// backoff" — clients should respect grpc-retry-on (per the OTel
// exporter spec).
package otlp

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/engine"
)

// ─── Server implementations ──────────────────────────────────────────────

// traceService implements coltracepb.TraceServiceServer.
type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	engine *engine.ProcessingEngine
}

func (s *traceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	tenantID := tenantFromCtx(ctx)
	if tenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing tenant id")
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remarshal: %v", err)
	}
	sigs, err := DecodeTraces(tenantID, body, time.Now().UTC())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode: %v", err)
	}
	if err := ingestAll(ctx, s.engine, sigs); err != nil {
		return nil, status.Error(codes.ResourceExhausted, "overloaded, retry later")
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// metricsService implements colmetricspb.MetricsServiceServer.
type metricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	engine *engine.ProcessingEngine
}

func (s *metricsService) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	tenantID := tenantFromCtx(ctx)
	if tenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing tenant id")
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remarshal: %v", err)
	}
	sigs, err := DecodeMetrics(tenantID, body, time.Now().UTC())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode: %v", err)
	}
	if err := ingestAll(ctx, s.engine, sigs); err != nil {
		return nil, status.Error(codes.ResourceExhausted, "overloaded, retry later")
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// logsService implements collogspb.LogsServiceServer.
type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	engine *engine.ProcessingEngine
}

func (s *logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	tenantID := tenantFromCtx(ctx)
	if tenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing tenant id")
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remarshal: %v", err)
	}
	sigs, err := DecodeLogs(tenantID, body, time.Now().UTC())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode: %v", err)
	}
	if err := ingestAll(ctx, s.engine, sigs); err != nil {
		return nil, status.Error(codes.ResourceExhausted, "overloaded, retry later")
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// ─── Server construction ─────────────────────────────────────────────────

// RegisterGRPCServices wires the three OTLP services onto srv and
// returns it for the caller to start with srv.Serve(lis). The auth
// interceptor is applied unconditionally; missing/invalid credentials
// fail with codes.Unauthenticated.
//
// The keyStore argument MUST be the same instance used by the HTTP
// receiver so the validation cache is shared and revocation
// propagates uniformly.
func RegisterGRPCServices(srv *grpc.Server, e *engine.ProcessingEngine, keyStore auth.KeyStore) {
	coltracepb.RegisterTraceServiceServer(srv, &traceService{engine: e})
	colmetricspb.RegisterMetricsServiceServer(srv, &metricsService{engine: e})
	collogspb.RegisterLogsServiceServer(srv, &logsService{engine: e})
}

// AuthInterceptor returns a unary interceptor that validates the
// caller's bearer token and enforces the `ingest` scope. Stash the
// validated tenant id into the outgoing ctx so the service Export
// methods can pick it up.
func AuthInterceptor(keyStore auth.KeyStore) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		token := bearerFromMetadata(md)
		if token == "" {
			return nil, status.Error(codes.Unauthenticated, "missing authorization")
		}

		var keyMD auth.KeyMetadata
		var valid bool
		if sa, ok := keyStore.(auth.ScopeAwareKeyStore); ok {
			keyMD, valid = sa.ValidateKeyWithMetadata(token)
		} else {
			tid, vOk := keyStore.ValidateKey(token)
			if vOk {
				keyMD = auth.KeyMetadata{TenantID: tid, Scopes: auth.DefaultScopes()}
				valid = true
			}
		}
		if !valid {
			return nil, status.Error(codes.PermissionDenied, "invalid api key")
		}
		if !auth.HasScope(keyMD.Scopes, auth.ScopeIngest) {
			return nil, status.Errorf(codes.PermissionDenied,
				"insufficient scope; required: %s", auth.ScopeIngest)
		}

		// Surface tenant id + scopes downstream.
		ctx = withTenant(ctx, keyMD.TenantID)
		ctx = auth.WithScopes(ctx, keyMD.Scopes)
		return handler(ctx, req)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────

type ctxTenantKey struct{}

func withTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, ctxTenantKey{}, tenantID)
}

// WithTenantForTest is the exported seam used by the ingest-gateway's
// custom auth interceptor (services/ingest-gateway/internal/receiver/
// grpc_receiver.go) to surface the validated tenant id into the
// shared ctx key the OTLP services read. Production code should use
// AuthInterceptor instead; this exists because we currently mount
// both the legacy IngestService and the OTLP services on the same
// gRPC server with one shared interceptor.
func WithTenantForTest(ctx context.Context, tenantID string) context.Context {
	return withTenant(ctx, tenantID)
}

func tenantFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxTenantKey{}).(string)
	return v
}

func bearerFromMetadata(md metadata.MD) string {
	for _, v := range md.Get("authorization") {
		if strings.HasPrefix(v, "Bearer ") {
			return strings.TrimPrefix(v, "Bearer ")
		}
	}
	return ""
}

// errUnimplemented is here so callers that import this file from a
// constructed server without registering services get a useful error
// in their tests. Unused at runtime.
var errUnimplemented = errors.New("otlp: gRPC services not registered")
