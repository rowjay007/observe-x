package otlp

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/engine"
)

// ─── Helpers ──────────────────────────────────────────────────────────────

func startGRPCServer(t *testing.T, e *engine.ProcessingEngine, ks auth.KeyStore) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(AuthInterceptor(ks)))
	RegisterGRPCServices(srv, e, ks)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), func() { srv.GracefulStop() }
}

func newTestEngine(t *testing.T) *engine.ProcessingEngine {
	t.Helper()
	dir, err := os.MkdirTemp("", "otlp-grpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	e, err := engine.NewProcessingEngineWithOptions(engine.Options{
		WALPath:          filepath.Join(dir, "wal"),
		IngestBufferSize: 1024,
		Workers:          1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e
}

func simpleTraceReq() *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{
				Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "svc"}}},
				},
			},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					TraceId:           []byte("0123456789abcdef"),
					SpanId:            []byte("01234567"),
					Name:              "op",
					StartTimeUnixNano: 1,
					EndTimeUnixNano:   2,
				}},
			}},
		}},
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────

func TestGRPCExportTracesHappyPath(t *testing.T) {
	e := newTestEngine(t)
	ks := auth.NewMemoryKeyStore()
	key := ks.AddWithScopes("acme", "raw-secret-1", []auth.Scope{auth.ScopeIngest})

	addr, stop := startGRPCServer(t, e, ks)
	defer stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := coltracepb.NewTraceServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)

	resp, err := client.Export(ctx, simpleTraceReq())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
}

func TestGRPCExportRejectsMissingAuth(t *testing.T) {
	e := newTestEngine(t)
	ks := auth.NewMemoryKeyStore()

	addr, stop := startGRPCServer(t, e, ks)
	defer stop()

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	client := coltracepb.NewTraceServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := client.Export(ctx, simpleTraceReq()); err == nil {
		t.Fatal("expected unauthenticated error")
	}
}

func TestGRPCExportRejectsInsufficientScope(t *testing.T) {
	e := newTestEngine(t)
	ks := auth.NewMemoryKeyStore()
	// QUERY-only key — cannot ingest.
	key := ks.AddWithScopes("acme", "raw-secret-2", []auth.Scope{auth.ScopeQuery})

	addr, stop := startGRPCServer(t, e, ks)
	defer stop()

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	client := coltracepb.NewTraceServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)

	if _, err := client.Export(ctx, simpleTraceReq()); err == nil {
		t.Fatal("expected PermissionDenied for insufficient scope")
	}
}
