package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Dispatcher behaviour ────────────────────────────────────────────────

type stubNotifier struct {
	name string
	fail bool
	hits atomic.Int32
}

func (s *stubNotifier) Name() string { return s.name }
func (s *stubNotifier) Send(_ context.Context, _ Notification) error {
	s.hits.Add(1)
	if s.fail {
		return errors.New("boom")
	}
	return nil
}

func TestDispatcherFansOutAndCollectsErrors(t *testing.T) {
	good := &stubNotifier{name: "a"}
	bad := &stubNotifier{name: "b", fail: true}
	d := NewDispatcher([]Notifier{good, bad})

	err := d.Dispatch(context.Background(), Notification{Title: "x"})
	if err == nil {
		t.Fatal("expected error from failing notifier")
	}
	if !strings.Contains(err.Error(), "b: boom") {
		t.Errorf("error should name failing notifier: %v", err)
	}
	if good.hits.Load() != 1 || bad.hits.Load() != 1 {
		t.Errorf("each notifier should have been called once: a=%d b=%d", good.hits.Load(), bad.hits.Load())
	}
}

func TestDispatcherEmptyIsNoOp(t *testing.T) {
	d := NewDispatcher(nil)
	if err := d.Dispatch(context.Background(), Notification{}); err != nil {
		t.Errorf("expected nil from empty dispatcher, got %v", err)
	}
}

func TestDispatcherRespectsPerNotifierTimeout(t *testing.T) {
	hang := func() Notifier {
		return notifierFn{name: "hang", send: func(ctx context.Context, _ Notification) error {
			<-ctx.Done()
			return ctx.Err()
		}}
	}
	d := NewDispatcher([]Notifier{hang()}, WithPerNotifierTimeout(20*time.Millisecond))

	start := time.Now()
	err := d.Dispatch(context.Background(), Notification{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("dispatcher took %s — per-notifier timeout not enforced", elapsed)
	}
}

type notifierFn struct {
	name string
	send func(ctx context.Context, n Notification) error
}

func (n notifierFn) Name() string                                   { return n.name }
func (n notifierFn) Send(ctx context.Context, x Notification) error { return n.send(ctx, x) }

// ─── Slack ────────────────────────────────────────────────────────────────

func TestSlackSendsMessage(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sn := NewSlackNotifier(srv.URL)

	err := sn.Send(context.Background(), Notification{
		Title: "Burning hot", Description: "5xx rate",
		Severity: SeverityCritical, TenantID: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatal(err)
	}
	text, _ := got["text"].(string)
	if !strings.Contains(text, "Burning hot") || !strings.Contains(text, "FIRING") || !strings.Contains(text, "acme") {
		t.Errorf("slack text missing context: %q", text)
	}
}

func TestSlackSurfacesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("bad token"))
	}))
	defer srv.Close()
	sn := NewSlackNotifier(srv.URL)
	if err := sn.Send(context.Background(), Notification{Title: "x"}); err == nil {
		t.Fatal("expected error on 403")
	}
}

// ─── PagerDuty ────────────────────────────────────────────────────────────

func TestPagerDutyShape(t *testing.T) {
	// Drop traffic on a local stub and verify the JSON we encode.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if got["routing_key"] != "rk" {
			t.Errorf("missing routing_key: %v", got)
		}
		if got["event_action"] != "trigger" {
			t.Errorf("event_action should be trigger: %v", got)
		}
		if got["dedup_key"] != "fp" {
			t.Errorf("dedup_key should propagate fingerprint: %v", got)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	pd := &PagerDutyNotifier{IntegrationKey: "rk", Client: srv.Client()}
	// Override the URL by exporting the field — we use the live URL in
	// production so we don't add a config option just for tests. The
	// simplest seam: re-implement Send via the stub server.
	if err := postOverride(pd, srv.URL, Notification{
		Fingerprint: "fp", Title: "x", Severity: SeverityCritical,
	}); err != nil {
		t.Fatal(err)
	}
}

// postOverride lets the PagerDuty test exercise Send against a stub
// server without exposing a URL field on the public struct. It's
// strictly test-scope.
func postOverride(p *PagerDutyNotifier, url string, n Notification) error {
	original := p.Client
	defer func() { p.Client = original }()
	p.Client = &http.Client{Transport: redirectingTransport{to: url, inner: http.DefaultTransport}}
	return p.Send(context.Background(), n)
}

type redirectingTransport struct {
	to    string
	inner http.RoundTripper
}

func (r redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace scheme+host with the stub.
	u, _ := http.NewRequest(req.Method, r.to, req.Body)
	u.Header = req.Header
	return r.inner.RoundTrip(u)
}

// ─── Webhook ──────────────────────────────────────────────────────────────

func TestWebhookSendsFullNotification(t *testing.T) {
	var captured Notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		_ = json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := NewWebhookNotifier(srv.URL, map[string]string{"X-Hook-Token": "secret"})
	src := Notification{
		Fingerprint: "fp1", TenantID: "acme",
		Title: "T", Description: "D",
		Severity: SeverityWarning,
		Labels:   map[string]string{"k": "v"},
	}
	if err := wh.Send(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if captured.Fingerprint != "fp1" || captured.Labels["k"] != "v" {
		t.Errorf("payload lost: %+v", captured)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}
