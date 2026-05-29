package receiver

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/signal"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"net"
	"strings"
	"sync"
	"time"
)

// GRPCReceiver implements a gRPC server for OTLP signal ingestion.
// It listens on a configurable address and validates API keys from
// gRPC metadata before routing signals to the processing engine.
type GRPCReceiver struct {
	addr      string
	server    *grpc.Server
	engine    *engine.ProcessingEngine
	keyStore  auth.KeyStore
	logger    *zap.Logger
	tlsConfig *tls.Config
	mu        sync.Mutex
	running   bool
}

// NewGRPCReceiver creates a new gRPC receiver bound to the given address.
func NewGRPCReceiver(addr string, eng *engine.ProcessingEngine, keyStore auth.KeyStore, logger *zap.Logger, tlsConfig *tls.Config) *GRPCReceiver {
	return &GRPCReceiver{
		addr:      addr,
		engine:    eng,
		keyStore:  keyStore,
		logger:    logger,
		tlsConfig: tlsConfig,
	}
}

// Start begins listening for gRPC connections. It registers the IngestService
// and blocks until the context is cancelled or Stop is called.
func (r *GRPCReceiver) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("gRPC receiver already running")
	}

	lis, err := net.Listen("tcp", r.addr)
	if err != nil {
		r.mu.Unlock()
		return fmt.Errorf("failed to listen on %s: %w", r.addr, err)
	}

	serverOpts := []grpc.ServerOption{grpc.UnaryInterceptor(r.authInterceptor)}
	if r.tlsConfig != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(r.tlsConfig)))
	}

	r.server = grpc.NewServer(serverOpts...)

	// Register the ingest service handler
	RegisterIngestServiceServer(r.server, &ingestServiceHandler{
		engine: r.engine,
		logger: r.logger,
	})

	r.running = true
	r.mu.Unlock()

	r.logger.Info("gRPC receiver started", zap.String("addr", r.addr))

	// Graceful shutdown on context cancellation
	go func() {
		<-ctx.Done()
		r.logger.Info("gRPC receiver shutting down")
		r.server.GracefulStop()
	}()

	if err := r.server.Serve(lis); err != nil {
		return fmt.Errorf("gRPC server error: %w", err)
	}
	return nil
}

// Stop gracefully stops the gRPC server.
func (r *GRPCReceiver) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server != nil {
		r.server.GracefulStop()
		r.running = false
	}
}

// authInterceptor is a unary gRPC interceptor that extracts and validates
// the API key from the "authorization" metadata field.
func (r *GRPCReceiver) authInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}

	authValues := md.Get("authorization")
	if len(authValues) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing authorization header")
	}

	authHeader := authValues[0]
	const bearerScheme = "Bearer "
	if !strings.HasPrefix(authHeader, bearerScheme) {
		return nil, status.Error(codes.Unauthenticated, "invalid authorization scheme")
	}

	key := authHeader[len(bearerScheme):]
	tenantID, valid := r.keyStore.ValidateKey(key)
	if !valid {
		return nil, status.Error(codes.PermissionDenied, "invalid api key")
	}

	// Inject tenant ID into context metadata for downstream handlers
	md.Set("x-tenant-id", tenantID)
	ctx = metadata.NewIncomingContext(ctx, md)

	return handler(ctx, req)
}

// ─── Inline gRPC service definition ────────────────────────────────────────
// We define the service interface and registration here so the receiver
// works without generated protobuf code. When protoc-generated code is
// available, these can be replaced by the generated stubs.

// IngestRequest represents a batch of signals for ingestion.
type IngestRequest struct {
	Signals []IngestSignal `json:"signals"`
}

// IngestSignal represents a single signal in an ingest request.
type IngestSignal struct {
	TenantID   string            `json:"tenant_id"`
	Type       string            `json:"type"`
	Payload    []byte            `json:"payload"`
	Attributes map[string]string `json:"attributes"`
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
}

// IngestResponse is the response to an ingest request.
type IngestResponse struct {
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	AcceptedCount int64  `json:"accepted_count"`
}

// IngestServiceServer is the server API for the IngestService.
type IngestServiceServer interface {
	Export(ctx context.Context, req *IngestRequest) (*IngestResponse, error)
}

// RegisterIngestServiceServer registers a handler for the IngestService.
func RegisterIngestServiceServer(s *grpc.Server, srv IngestServiceServer) {
	s.RegisterService(&_IngestService_serviceDesc, srv)
}

var _IngestService_serviceDesc = grpc.ServiceDesc{
	ServiceName: "ingest.v1.IngestService",
	HandlerType: (*IngestServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Export",
			Handler:    _IngestService_Export_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "ingest/v1/ingest.proto",
}

func _IngestService_Export_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(IngestRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(IngestServiceServer).Export(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/ingest.v1.IngestService/Export",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(IngestServiceServer).Export(ctx, req.(*IngestRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// ─── Service implementation ────────────────────────────────────────────────

type ingestServiceHandler struct {
	engine *engine.ProcessingEngine
	logger *zap.Logger
}

func (h *ingestServiceHandler) Export(ctx context.Context, req *IngestRequest) (*IngestResponse, error) {
	// Extract tenant ID from metadata (set by auth interceptor)
	md, _ := metadata.FromIncomingContext(ctx)
	tenantIDs := md.Get("x-tenant-id")
	tenantID := ""
	if len(tenantIDs) > 0 {
		tenantID = tenantIDs[0]
	}

	var accepted int64
	for _, s := range req.Signals {
		sigTenantID := tenantID
		if sigTenantID == "" {
			sigTenantID = s.TenantID
		}

		sig := signal.Signal{
			TenantID:   sigTenantID,
			Type:       parseSignalType(s.Type),
			Payload:    s.Payload,
			Attributes: s.Attributes,
			ReceivedAt: time.Now(),
		}

		if err := h.engine.ProcessSignal(ctx, sig); err != nil {
			h.logger.Warn("failed to process signal",
				zap.String("tenant_id", sigTenantID),
				zap.Error(err),
			)
			continue
		}
		accepted++
	}

	return &IngestResponse{
		Success:       true,
		Message:       "signals accepted",
		AcceptedCount: accepted,
	}, nil
}

// parseSignalType converts a string type to the signal.Type enum.
func parseSignalType(t string) signal.Type {
	switch strings.ToUpper(t) {
	case "METRIC", "SIGNAL_TYPE_METRIC", "1":
		return signal.Metric
	case "LOG", "SIGNAL_TYPE_LOG", "2":
		return signal.Log
	case "TRACE", "SIGNAL_TYPE_TRACE", "3":
		return signal.Trace
	case "PROFILE", "SIGNAL_TYPE_PROFILE", "4":
		return signal.Profile
	default:
		return signal.Metric
	}
}
