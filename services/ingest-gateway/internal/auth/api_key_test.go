package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateAndValidateAPIKey(t *testing.T) {
	secret := "test-secret"
	tenantID := "tenant-abc"
	apiKey := GenerateAPIKey(secret, tenantID)

	validator := NewStatelessKeyValidator(secret)
	validatedTenant, valid := validator.ValidateKey(apiKey)
	if !valid {
		t.Fatalf("expected valid API key")
	}
	if validatedTenant != tenantID {
		t.Fatalf("expected tenant %s, got %s", tenantID, validatedTenant)
	}
}

func TestValidateKeyInvalidKey(t *testing.T) {
	validator := NewStatelessKeyValidator("another-secret")
	_, valid := validator.ValidateKey("invalidformat")
	if valid {
		t.Fatal("expected malformed key to be invalid")
	}

	_, valid = validator.ValidateKey("tenant:wronghash")
	if valid {
		t.Fatal("expected wrong hash to be invalid")
	}
}

func TestMemoryKeyStoreLifecycle(t *testing.T) {
	store := NewMemoryKeyStore()
	tenantID := "acme"
	key := store.Add(tenantID, "raw-secret-123")

	gotTenant, valid := store.ValidateKey(key)
	if !valid || gotTenant != tenantID {
		t.Fatalf("expected valid key for %s, got valid=%v tenant=%q", tenantID, valid, gotTenant)
	}

	store.Revoke(key)
	if _, valid := store.ValidateKey(key); valid {
		t.Fatal("expected revoked key to be invalid")
	}
}

func TestMemoryKeyStoreRejectsTenantSpoof(t *testing.T) {
	store := NewMemoryKeyStore()
	key := store.Add("acme", "raw-secret")
	// Tamper with the tenant id segment of an otherwise-valid key.
	parts := strings.SplitN(key, ":", 2)
	spoofed := "evil:" + parts[1]
	if _, valid := store.ValidateKey(spoofed); valid {
		t.Fatal("expected spoofed tenant prefix to be rejected")
	}
}

func TestAuthMiddleware(t *testing.T) {
	secret := "auth-secret"
	validator := NewStatelessKeyValidator(secret)
	middleware := NewAuthMiddleware(validator)

	tenantID := "tenant-123"
	apiKey := GenerateAPIKey(secret, tenantID)

	handler := middleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-ID") != tenantID {
			http.Error(w, "tenant mismatch", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.Code)
	}
}
