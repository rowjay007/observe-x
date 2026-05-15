package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
