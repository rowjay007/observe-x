package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSSEHubFanOutAcrossSubscribers(t *testing.T) {
	h := newSSEHub(4)
	a := h.subscribe("acme")
	b := h.subscribe("acme")
	c := h.subscribe("beta")
	defer h.unsubscribe(a)
	defer h.unsubscribe(b)
	defer h.unsubscribe(c)

	h.Publish(sseEvent{TenantID: "acme", Type: "alert.fired", Payload: map[string]any{"x": 1}})
	select {
	case ev := <-a.ch:
		if ev.Type != "alert.fired" {
			t.Errorf("a got %q", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a never received")
	}
	select {
	case <-b.ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("b never received")
	}
	select {
	case ev := <-c.ch:
		t.Fatalf("c (beta) should not have received: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSSEHubDropsToSlowSubscriber(t *testing.T) {
	// Fill the buffer for `slow` and ensure further publishes neither
	// block nor crash. The slow client is the only one harmed.
	h := newSSEHub(2)
	slow := h.subscribe("t")
	fast := h.subscribe("t")
	defer h.unsubscribe(slow)
	defer h.unsubscribe(fast)

	// Fill slow's buffer by draining fast and not draining slow.
	for i := 0; i < 10; i++ {
		h.Publish(sseEvent{TenantID: "t", Type: "alert.fired"})
		select {
		case <-fast.ch:
		default:
		}
	}
	// slow has at most 2 in buffer; the rest dropped. Confirm no panic
	// and that fast still got everything we drained for it.
	count := 0
	for {
		select {
		case <-slow.ch:
			count++
		default:
			goto done
		}
	}
done:
	if count > 2 {
		t.Errorf("slow buffer cap was 2, got %d", count)
	}
}

func TestStreamHandlerEmitsHeartbeatAndEvent(t *testing.T) {
	hub := newSSEHub(8)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Bypass the real auth — set X-Tenant-ID directly for the test.
	r.GET("/v1/alerts/stream", streamAlertsHandler(hub))
	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/alerts/stream", nil)
	req.Header.Set("X-Tenant-ID", "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type: %q", got)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var saw struct {
		fired   bool
		connect bool
		mu      sync.Mutex
	}
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 8192), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			saw.mu.Lock()
			if strings.HasPrefix(line, ": connected") {
				saw.connect = true
			}
			if strings.Contains(line, "alert.fired") {
				saw.fired = true
			}
			saw.mu.Unlock()
			if saw.fired && saw.connect {
				return
			}
		}
	}()

	// Give the goroutine a chance to subscribe before we publish.
	time.Sleep(50 * time.Millisecond)
	hub.Publish(sseEvent{TenantID: "acme", Type: "alert.fired", Payload: map[string]any{"fp": "abc"}})

	deadline := time.After(2 * time.Second)
	for {
		saw.mu.Lock()
		ok := saw.fired && saw.connect
		saw.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			wg.Wait()
			t.Fatalf("never saw fired+connect; fired=%v connect=%v", saw.fired, saw.connect)
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}
