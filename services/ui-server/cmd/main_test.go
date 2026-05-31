package main

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEmbeddedSPALoads verifies the embed.FS is wired and the SPA's
// three key assets (index.html, app.js, app.css) are reachable.
// This is a structural smoke test so we don't ship an unloadable UI.
func TestEmbeddedSPALoads(t *testing.T) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(spaHandler(sub))
	defer srv.Close()

	cases := []struct {
		path   string
		expect string
	}{
		{"/", "ObserveX"},                // index.html
		{"/index.html", "ObserveX"},      //
		{"/app.css", ":root"},            // CSS variables block
		{"/app.js", "ObserveX"},          // app comment
		{"/does-not-exist", "ObserveX"},  // SPA fallback to index
	}
	for _, c := range cases {
		r, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", c.path, r.StatusCode)
			continue
		}
		if !strings.Contains(string(body), c.expect) {
			t.Errorf("GET %s: missing %q in body (len=%d)", c.path, c.expect, len(body))
		}
	}
}

func TestSecurityHeadersOnSPA(t *testing.T) {
	sub, _ := fs.Sub(assetsFS, "assets")
	srv := httptest.NewServer(spaHandler(sub))
	defer srv.Close()

	r, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Referrer-Policy"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing security header %q", h)
		}
	}
}

func TestSpaHandlerRejectsTraversal(t *testing.T) {
	sub, _ := fs.Sub(assetsFS, "assets")
	srv := httptest.NewServer(spaHandler(sub))
	defer srv.Close()

	r, err := http.Get(srv.URL + "/..%2F..%2Fetc%2Fpasswd")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	// Either we 400, or we serve index.html. We must NOT serve /etc/passwd.
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), "root:") {
		t.Fatal("UI served /etc/passwd via path traversal!")
	}
}

func TestDirectorStripsPrefix(t *testing.T) {
	// Build an upstream that records what URL it saw.
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Run the proxy with no validator (dev path).
	h := proxyHandler(upstream.URL, "/api/tenant", nil, nopLogger())
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, err := http.Get(srv.URL + "/api/tenant/v1/tenants")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if seen != "/v1/tenants" {
		t.Errorf("upstream saw %q; want /v1/tenants", seen)
	}
}
