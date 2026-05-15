package receiver

import (
	"encoding/json"
	"github.com/rowjay007/observe-x/pkg/signal"
	"net/http"
	"sync"
)

type HTTPReceiver struct {
	mu         sync.Mutex
	signalChan chan<- signal.Signal
}

func NewHTTPReceiver(signalChan chan<- signal.Signal) *HTTPReceiver {
	return &HTTPReceiver{
		signalChan: signalChan,
	}
}

func (r *HTTPReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		TenantID   string            `json:"tenant_id"`
		Type       string            `json:"type"`
		Data       json.RawMessage   `json:"data"`
		Attributes map[string]string `json:"attributes"`
	}

	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sig := signal.Signal{
		TenantID:   payload.TenantID,
		Type:       signal.Type(payload.Type),
		Payload:    payload.Data,
		Attributes: payload.Attributes,
	}

	select {
	case r.signalChan <- sig:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	case <-req.Context().Done():
		http.Error(w, "request cancelled", http.StatusRequestTimeout)
	default:
		http.Error(w, "receiver full", http.StatusServiceUnavailable)
	}
}
