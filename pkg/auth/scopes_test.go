package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseScopes(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantErr bool
		want    []Scope
	}{
		{name: "empty defaults to ingest", in: nil, want: []Scope{ScopeIngest}},
		{name: "single valid", in: []string{"query"}, want: []Scope{ScopeQuery}},
		{name: "case-insensitive", in: []string{"INGEST", " Query "}, want: []Scope{ScopeIngest, ScopeQuery}},
		{name: "dedup + sort", in: []string{"query", "ingest", "query"}, want: []Scope{ScopeIngest, ScopeQuery}},
		{name: "unknown rejected", in: []string{"ingest", "wat"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseScopes(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if !equalScopes(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestHasScope(t *testing.T) {
	granted := []Scope{ScopeIngest, ScopeQuery}
	if !HasScope(granted, ScopeQuery) {
		t.Error("query should be allowed")
	}
	if HasScope(granted, ScopeAlertWrite) {
		t.Error("alert.write should NOT be allowed")
	}
	if !HasScope(granted, ScopeIngest, ScopeQuery) {
		t.Error("multi-scope AND should pass when all granted")
	}
	if HasScope(granted, ScopeIngest, ScopeAlertWrite) {
		t.Error("multi-scope AND must fail when one missing")
	}
	// Empty required ⇒ always allow (consistent with middleware "no
	// scope guard" path).
	if !HasScope(granted) {
		t.Error("empty required should allow")
	}
}

func TestAuthMiddlewareSetsScopesInContext(t *testing.T) {
	store := NewMemoryKeyStore()
	key := store.AddWithScopes("acme", "raw", []Scope{ScopeIngest, ScopeQuery})
	mw := NewAuthMiddleware(store)

	var captured []Scope
	h := mw.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = ScopesFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	if !equalScopes(captured, []Scope{ScopeIngest, ScopeQuery}) {
		t.Errorf("scopes in ctx: %v", captured)
	}
}

func TestRequireScope_AllowsAndDenies(t *testing.T) {
	store := NewMemoryKeyStore()
	keyIngestOnly := store.AddWithScopes("acme", "raw1", []Scope{ScopeIngest})
	keyQuery := store.AddWithScopes("acme", "raw2", []Scope{ScopeIngest, ScopeQuery})

	mw := NewAuthMiddleware(store)
	guard := mw.RequireScope(ScopeQuery)
	handler := mw.Middleware(guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// ingest-only ⇒ 403 with WWW-Authenticate
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+keyIngestOnly)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing scope, got %d", rec.Code)
	}
	if w := rec.Header().Get("WWW-Authenticate"); !strings.Contains(w, "query") {
		t.Errorf("WWW-Authenticate missing scope hint: %q", w)
	}

	// ingest+query ⇒ 200
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+keyQuery)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with sufficient scope, got %d", rec.Code)
	}
}

func TestStatelessValidatorGrantsAllScopes(t *testing.T) {
	v := NewStatelessKeyValidator("dev")
	md, ok := v.ValidateKeyWithMetadata(GenerateAPIKey("dev", "acme"))
	if !ok {
		t.Fatal("expected valid key")
	}
	if !HasScope(md.Scopes, ScopeIngest, ScopeQuery, ScopeAlertWrite, ScopeTenantAdmin) {
		t.Errorf("stateless validator should grant all scopes (dev only): %v", md.Scopes)
	}
}

func TestMemoryKeyStoreScopesIsolation(t *testing.T) {
	s := NewMemoryKeyStore()
	keyA := s.AddWithScopes("acme", "ra", []Scope{ScopeIngest})
	keyB := s.AddWithScopes("acme", "rb", []Scope{ScopeQuery})

	mdA, _ := s.ValidateKeyWithMetadata(keyA)
	mdB, _ := s.ValidateKeyWithMetadata(keyB)
	if !equalScopes(mdA.Scopes, []Scope{ScopeIngest}) {
		t.Errorf("A scopes: %v", mdA.Scopes)
	}
	if !equalScopes(mdB.Scopes, []Scope{ScopeQuery}) {
		t.Errorf("B scopes: %v", mdB.Scopes)
	}
}

func TestScopesContextFallback(t *testing.T) {
	// Missing scope set ⇒ DefaultScopes() (safe-by-default for handlers
	// that forget to mount the middleware).
	got := ScopesFromContext(context.Background())
	if !equalScopes(got, DefaultScopes()) {
		t.Errorf("fallback should equal DefaultScopes; got %v", got)
	}
}

func equalScopes(a, b []Scope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
