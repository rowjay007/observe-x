package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ─── In-process IdP for tests ────────────────────────────────────────────

type fakeIDP struct {
	priv *rsa.PrivateKey
	kid  string
	srv  *httptest.Server
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{priv: priv, kid: "k1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   idp.issuer(),
			"jwks_uri": idp.srv.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwk := jose.JSONWebKey{Key: priv.Public(), KeyID: idp.kid, Algorithm: "RS256", Use: "sig"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
	})
	idp.srv = httptest.NewServer(mux)
	return idp
}

func (i *fakeIDP) issuer() string { return i.srv.URL }
func (i *fakeIDP) Close()         { i.srv.Close() }

func (i *fakeIDP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.kid),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ─── Tests ───────────────────────────────────────────────────────────────

func TestValidatorHappyPath(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()

	v, err := NewValidator(context.Background(), Config{
		Issuer:   idp.issuer(),
		Audience: "observex",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	now := time.Now()
	tok := idp.sign(t, map[string]any{
		"iss":    idp.issuer(),
		"aud":    "observex",
		"sub":    "user-42",
		"email":  "alice@example.com",
		"iat":    now.Unix(),
		"exp":    now.Add(10 * time.Minute).Unix(),
		"groups": []string{"sre", "observex-admin"},
	})

	claims, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Errorf("sub: %q", claims.Subject)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("email: %q", claims.Email)
	}
	if len(claims.Groups) != 2 {
		t.Errorf("groups: %v", claims.Groups)
	}
}

func TestValidatorRejectsWrongAudience(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{
		Issuer: idp.issuer(), Audience: "observex",
	})
	defer v.Close()

	tok := idp.sign(t, map[string]any{
		"iss": idp.issuer(),
		"aud": "wrong-aud",
		"sub": "x",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Validate(context.Background(), tok); !IsAuthn(err) {
		t.Fatalf("expected IsAuthn, got %v", err)
	}
}

func TestValidatorRejectsExpired(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{
		Issuer: idp.issuer(), Audience: "observex", Skew: time.Second,
	})
	defer v.Close()

	tok := idp.sign(t, map[string]any{
		"iss": idp.issuer(), "aud": "observex", "sub": "x",
		"exp": time.Now().Add(-10 * time.Minute).Unix(),
	})
	if _, err := v.Validate(context.Background(), tok); !IsAuthn(err) {
		t.Fatalf("expected expired ⇒ IsAuthn, got %v", err)
	}
}

func TestValidatorRejectsInsufficientGroup(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{
		Issuer:      idp.issuer(),
		Audience:    "observex",
		AdminGroups: []string{"observex-admin"},
	})
	defer v.Close()

	tok := idp.sign(t, map[string]any{
		"iss":    idp.issuer(),
		"aud":    "observex",
		"sub":    "x",
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"random-user"},
	})
	_, err := v.Validate(context.Background(), tok)
	if !IsAuthz(err) {
		t.Fatalf("expected IsAuthz, got %v", err)
	}
}

func TestValidatorAcceptsGroupClaimOverride(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{
		Issuer:      idp.issuer(),
		Audience:    "observex",
		AdminGroups: []string{"sre"},
		GroupClaim:  "https://my-org/roles",
	})
	defer v.Close()

	tok := idp.sign(t, map[string]any{
		"iss":                  idp.issuer(),
		"aud":                  "observex",
		"sub":                  "x",
		"exp":                  time.Now().Add(time.Hour).Unix(),
		"https://my-org/roles": []string{"sre"},
		"groups":               []string{"not-this-one"},
	})
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("expected ok: %v", err)
	}
}

func TestMiddlewareMissingAuthIs401(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{Issuer: idp.issuer(), Audience: "observex"})
	defer v.Close()

	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddlewarePropagatesSubject(t *testing.T) {
	idp := newFakeIDP(t)
	defer idp.Close()
	v, _ := NewValidator(context.Background(), Config{Issuer: idp.issuer(), Audience: "observex"})
	defer v.Close()

	tok := idp.sign(t, map[string]any{
		"iss": idp.issuer(), "aud": "observex", "sub": "ops-bot",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	var captured string
	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("X-Operator-Subject")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if captured != "ops-bot" {
		t.Errorf("subject: %q", captured)
	}
}
