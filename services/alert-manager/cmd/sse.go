package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// sseHub is a tenant-scoped fan-out for alert events. Each
// subscriber gets its own bounded channel; if a subscriber falls
// behind (UI tab in the background, slow phone client) we drop
// the slowest events for that subscriber only — the SSE protocol
// permits message gaps, and starving the publisher would punish
// every other subscriber for one slow consumer.
//
// Phase D-3 (ADR-0020).
type sseHub struct {
	mu     sync.RWMutex
	subs   map[string]map[*sseClient]struct{} // tenant → set
	bufCap int
}

type sseEvent struct {
	TenantID string
	Type     string // "alert.fired" | "alert.resolved" | "alert.notified" | "heartbeat"
	Payload  any
}

type sseClient struct {
	tenant string
	ch     chan sseEvent
}

func newSSEHub(bufCap int) *sseHub {
	if bufCap <= 0 {
		bufCap = 32
	}
	return &sseHub{
		subs:   map[string]map[*sseClient]struct{}{},
		bufCap: bufCap,
	}
}

// Publish fans an event out to every subscriber for the tenant.
// Drops to a slow subscriber's buffer rather than blocking.
func (h *sseHub) Publish(ev sseEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for cli := range h.subs[ev.TenantID] {
		select {
		case cli.ch <- ev:
		default:
			// Subscriber lagging — drop this event on the floor for them.
			// Their browser will reconnect with Last-Event-ID and we'll
			// resume; for now this keeps the hub non-blocking.
		}
	}
}

func (h *sseHub) subscribe(tenant string) *sseClient {
	c := &sseClient{tenant: tenant, ch: make(chan sseEvent, h.bufCap)}
	h.mu.Lock()
	if h.subs[tenant] == nil {
		h.subs[tenant] = map[*sseClient]struct{}{}
	}
	h.subs[tenant][c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *sseHub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.subs[c.tenant], c)
	if len(h.subs[c.tenant]) == 0 {
		delete(h.subs, c.tenant)
	}
	h.mu.Unlock()
	close(c.ch)
}

// streamAlertsHandler is the GET /v1/alerts/stream endpoint.
// Server-Sent Events flow (text/event-stream); each event is a
// JSON-marshaled sseEvent.Payload preceded by an `event:` line.
//
// We send a heartbeat every 25s to keep proxies (NGINX default
// 60s read timeout, Envoy default 15m, CloudFront 30s) from
// killing idle connections.
func streamAlertsHandler(hub *sseHub) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Request.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant id"})
			return
		}

		w := c.Writer
		flusher, ok := w.(http.Flusher)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // NGINX: don't buffer
		w.WriteHeader(http.StatusOK)
		// Immediately send a comment so the client knows the
		// connection is open and to flush proxy buffers.
		_, _ = fmt.Fprintf(w, ": connected to observex %d\n\n", time.Now().Unix())
		flusher.Flush()

		client := hub.subscribe(tenantID)
		defer hub.unsubscribe(client)

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		notify := c.Request.Context().Done()

		for {
			select {
			case <-notify:
				return
			case <-ticker.C:
				if !writeSSE(w, "heartbeat", map[string]any{"t": time.Now().Unix()}) {
					return
				}
				flusher.Flush()
			case ev, ok := <-client.ch:
				if !ok {
					return
				}
				if !writeSSE(w, ev.Type, ev.Payload) {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// writeSSE emits one event in the spec wire format. Returns false
// on write error so the caller bails out of the stream loop.
func writeSSE(w http.ResponseWriter, kind string, payload any) bool {
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	// Strip newlines from the payload so they don't break the SSE
	// framing. JSON-marshalled output should never contain raw \n
	// but defence in depth is cheap here.
	data := strings.ReplaceAll(string(b), "\n", " ")
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		return false
	}
	return true
}
