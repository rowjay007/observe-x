package otlp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/rowjay007/observe-x/pkg/engine"
	"github.com/rowjay007/observe-x/pkg/signal"
)

// HandleTracePayload is kept for backward compatibility with Phase A
// callers; it decodes the body via DecodeTraces and pushes every Signal
// through the engine. The HTTP handler below is the preferred entrypoint.
func HandleTracePayload(ctx context.Context, e *engine.ProcessingEngine, tenantID string, payload []byte) error {
	if e == nil {
		return errors.New("otlp: nil engine")
	}
	if tenantID == "" {
		return errors.New("otlp: missing tenant id")
	}
	signals, err := DecodeTraces(tenantID, payload, time.Now().UTC())
	if err != nil {
		return err
	}
	return ingestAll(ctx, e, signals)
}

// ─── HTTP handlers ────────────────────────────────────────────────────────

// Handler exposes one HTTP handler per OTLP signal type. Construct with
// NewHandler and mount the three Handle* methods on /v1/traces,
// /v1/metrics, /v1/logs respectively.
type Handler struct {
	engine *engine.ProcessingEngine
}

func NewHandler(e *engine.ProcessingEngine) *Handler {
	return &Handler{engine: e}
}

// HandleTraces decodes an OTLP/HTTP traces request and ingests every
// span as a Signal. Replies with an empty ExportTraceServiceResponse
// (status 200) on success, per the OTLP spec.
func (h *Handler) HandleTraces(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, signal.Trace, func(tid string, body []byte, ts time.Time) ([]signal.Signal, []byte, error) {
		sigs, err := DecodeTraces(tid, body, ts)
		if err != nil {
			return nil, nil, err
		}
		resp, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		return sigs, resp, nil
	})
}

func (h *Handler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, signal.Metric, func(tid string, body []byte, ts time.Time) ([]signal.Signal, []byte, error) {
		sigs, err := DecodeMetrics(tid, body, ts)
		if err != nil {
			return nil, nil, err
		}
		resp, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{})
		return sigs, resp, nil
	})
}

func (h *Handler) HandleLogs(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, signal.Log, func(tid string, body []byte, ts time.Time) ([]signal.Signal, []byte, error) {
		sigs, err := DecodeLogs(tid, body, ts)
		if err != nil {
			return nil, nil, err
		}
		resp, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		return sigs, resp, nil
	})
}

// ─── shared HTTP plumbing ─────────────────────────────────────────────────

type decodeFn func(tenantID string, body []byte, ts time.Time) (sigs []signal.Signal, response []byte, err error)

func (h *Handler) handle(w http.ResponseWriter, r *http.Request, _ signal.Type, decode decodeFn) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		http.Error(w, "missing tenant id", http.StatusUnauthorized)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/x-protobuf" && contentType != "application/protobuf" {
		http.Error(w, "unsupported content-type; expected application/x-protobuf",
			http.StatusUnsupportedMediaType)
		return
	}

	body, err := ReadBody(r.Body, r.Header.Get("Content-Encoding"))
	if errors.Is(err, ErrBodyTooLarge) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sigs, resp, err := decode(tenantID, body, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := ingestAll(r.Context(), h.engine, sigs); err != nil {
		// Engine back-pressure or transport failure → 429 lets the
		// SDK retry with backoff per the spec.
		http.Error(w, "overloaded, retry later", http.StatusTooManyRequests)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func ingestAll(ctx context.Context, e *engine.ProcessingEngine, sigs []signal.Signal) error {
	if e == nil {
		return errors.New("otlp: nil engine")
	}
	for _, sig := range sigs {
		if err := e.ProcessSignal(ctx, sig); err != nil {
			return err
		}
	}
	return nil
}
