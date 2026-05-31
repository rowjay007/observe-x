package alertsink

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/cep"
)

func TestInMemorySinkRecordsEvents(t *testing.T) {
	s := NewInMemorySink()
	s.Publish(cep.Event{Type: cep.HighErrorRate, TenantID: "acme"})
	s.Publish(cep.Event{Type: cep.HighLatency, TenantID: "acme"})
	if s.Len() != 2 {
		t.Fatalf("want 2 events, got %d", s.Len())
	}
	snap := s.Snapshot()
	if snap[0].Type != cep.HighErrorRate || snap[1].Type != cep.HighLatency {
		t.Errorf("event order lost: %+v", snap)
	}
}

func TestHTTPSinkPostsToEndpoint(t *testing.T) {
	var received atomic.Int32
	var lastBody atomic.Value // []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-ID") != "acme" {
			t.Errorf("missing tenant header: %v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(body)
		received.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sink := NewHTTPSink(HTTPOptions{
		Endpoint: srv.URL + "/v1/events",
		APIKey:   "test-key",
	})
	defer sink.Stop()

	sink.Publish(cep.Event{
		Type: cep.HighErrorRate, TenantID: "acme",
		Timestamp: time.Now(),
		Data:      map[string]any{"service": "api", "error_rate": 5.5},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if received.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if received.Load() < 1 {
		t.Fatalf("HTTPSink did not POST within deadline")
	}

	var wire map[string]any
	if err := json.Unmarshal(lastBody.Load().([]byte), &wire); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if wire["rule_id"] != "cep:HIGH_ERROR_RATE" {
		t.Errorf("rule_id wrong: %v", wire["rule_id"])
	}
	if wire["severity"] != "critical" {
		t.Errorf("severity should be critical for HighErrorRate: %v", wire["severity"])
	}
	labels, _ := wire["labels"].(map[string]any)
	if labels["service"] != "api" {
		t.Errorf("service label lost: %v", labels)
	}
}

func TestHTTPSinkDropsWhenBufferFull(t *testing.T) {
	// Server that hangs just long enough to fill the buffer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	sink := NewHTTPSink(HTTPOptions{
		Endpoint:       srv.URL,
		BufferSize:     1,
		RequestTimeout: 50 * time.Millisecond, // 50ms < server's 500ms ⇒ requests fail fast
	})
	defer sink.Stop()

	// Fire a tight flood; expect drops once the buffer fills.
	for i := 0; i < 200; i++ {
		sink.Publish(cep.Event{Type: cep.HighLatency, TenantID: "acme"})
	}
	_, dropped, _ := sink.Stats()
	if dropped == 0 {
		t.Errorf("expected drops on flood, got 0")
	}
}
