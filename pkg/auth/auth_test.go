package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateAndValidateAPIKey(t *testing.T) {
	const (
		secret   = "test-secret"
		tenantID = "tenant-abc"
	)
	apiKey := GenerateAPIKey(secret, tenantID)

	v := NewStatelessKeyValidator(secret)
	got, ok := v.ValidateKey(apiKey)
	if !ok {
		t.Fatal("expected valid api key")
	}
	if got != tenantID {
		t.Fatalf("tenant: want %q got %q", tenantID, got)
	}
}

func TestValidateKeyMalformed(t *testing.T) {
	v := NewStatelessKeyValidator("any-secret")
	if _, ok := v.ValidateKey("nothing-here"); ok {
		t.Fatal("expected malformed key to be invalid")
	}
	if _, ok := v.ValidateKey("tenant:wronghash"); ok {
		t.Fatal("expected wrong hash to be invalid")
	}
}

func TestAuthMiddleware(t *testing.T) {
	const (
		secret   = "auth-secret"
		tenantID = "tenant-123"
	)
	v := NewStatelessKeyValidator(secret)
	mw := NewAuthMiddleware(v)

	handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-ID") != tenantID {
			http.Error(w, "tenant mismatch", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+GenerateAPIKey(secret, tenantID))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestMemoryKeyStoreLifecycle(t *testing.T) {
	store := NewMemoryKeyStore()
	key := store.Add("acme", "raw-secret-123")
	got, ok := store.ValidateKey(key)
	if !ok || got != "acme" {
		t.Fatalf("expected valid; got tenant=%q valid=%v", got, ok)
	}
	store.Revoke(key)
	if _, ok := store.ValidateKey(key); ok {
		t.Fatal("expected revoked key to be invalid")
	}
}

func TestMemoryKeyStoreRejectsTenantSpoof(t *testing.T) {
	store := NewMemoryKeyStore()
	key := store.Add("acme", "raw-secret")
	parts := strings.SplitN(key, ":", 2)
	spoofed := "evil:" + parts[1]
	if _, ok := store.ValidateKey(spoofed); ok {
		t.Fatal("expected spoofed tenant prefix to be rejected")
	}
}

func TestArgon2idRoundTrip(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	encoded, err := hashArgon2id(secret)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !verifyArgon2id(secret, encoded) {
		t.Fatal("verify with correct secret failed")
	}
	if verifyArgon2id("wrong", encoded) {
		t.Fatal("verify with wrong secret accepted")
	}
}

func TestSplitWireKey(t *testing.T) {
	cases := []struct {
		in     string
		ok     bool
		tenant string
	}{
		{"acme:abc123:secret", true, "acme"},
		{"acme:abc123", false, ""},
		{":kid:secret", false, ""},
		{"acme::secret", false, ""},
		{"acme:kid:", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		gotTenant, _, _, gotOK := splitWireKey(c.in)
		if gotOK != c.ok {
			t.Errorf("%q: ok want %v got %v", c.in, c.ok, gotOK)
		}
		if c.ok && gotTenant != c.tenant {
			t.Errorf("%q: tenant want %q got %q", c.in, c.tenant, gotTenant)
		}
	}
}
