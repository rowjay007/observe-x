//go:build integration

// tenant_api_test exercises the full Phase-B-1 control plane against a
// live Postgres. It is gated by the `integration` build tag so plain
// `go test ./...` doesn't try to connect to a database that may not be
// up. CI sets `-tags=integration` after starting the `tests/docker-
// compose.yml` services.
//
// Skipped automatically if OBSERVE_X_POSTGRES_URL is not set.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/auth"
	"github.com/rowjay007/observe-x/services/tenant-api/store"
)

func TestTenantAPIFullLifecycle(t *testing.T) {
	dsn := os.Getenv("OBSERVE_X_POSTGRES_URL")
	if dsn == "" {
		t.Skip("OBSERVE_X_POSTGRES_URL not set; skipping Postgres integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ks, err := auth.NewPostgresKeyStore(ctx, dsn, auth.PostgresOptions{
		CacheTTL: 100 * time.Millisecond, // tighten for the revoke window test
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = ks.Close() }()

	if err := store.ApplyMigrations(ctx, ks.Pool()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tenantID := fmt.Sprintf("test-%d", time.Now().UnixNano())

	// Clean up any leftovers from a prior failed run.
	t.Cleanup(func() {
		_, _ = ks.Pool().Exec(context.Background(),
			`DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	repo := store.New(ks.Pool())

	// ── Create tenant ───────────────────────────────────────────────
	tn, err := repo.CreateTenant(ctx, store.Tenant{
		ID: tenantID, DisplayName: "Test", Tier: "free",
		RetentionDays: 14, QuotaEPS: 1000,
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if tn.ID != tenantID {
		t.Fatalf("unexpected tenant id %q", tn.ID)
	}

	// ── Issue API key with explicit scopes ─────────────────────────
	issued, err := ks.IssueKeyWithScopes(ctx, tenantID,
		[]auth.Scope{auth.ScopeIngest, auth.ScopeQuery}, nil)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if !strings.HasPrefix(issued.Raw, tenantID+":") {
		t.Fatalf("issued key prefix wrong: %q", issued.Raw)
	}
	if len(strings.Split(issued.Raw, ":")) != 3 {
		t.Fatalf("issued key must be 3-part: %q", issued.Raw)
	}
	if !auth.HasScope(issued.Scopes, auth.ScopeIngest, auth.ScopeQuery) {
		t.Fatalf("issued.Scopes lost expected scopes: %v", issued.Scopes)
	}

	// ── ValidateKeyWithMetadata: scopes round-trip ──────────────────
	md, ok := ks.ValidateKeyWithMetadata(issued.Raw)
	if !ok {
		t.Fatal("metadata validate should succeed")
	}
	if !auth.HasScope(md.Scopes, auth.ScopeIngest, auth.ScopeQuery) {
		t.Errorf("validated scopes lost: %v", md.Scopes)
	}
	if auth.HasScope(md.Scopes, auth.ScopeAlertWrite) {
		t.Errorf("validated scopes leaked alert.write: %v", md.Scopes)
	}

	// ── Validate key (cache miss → Argon2id verify) ────────────────
	got, ok := ks.ValidateKey(issued.Raw)
	if !ok {
		t.Fatal("expected fresh key to validate")
	}
	if got != tenantID {
		t.Fatalf("tenant mismatch: %q", got)
	}

	// ── Validate again (cache hit) ─────────────────────────────────
	if _, ok := ks.ValidateKey(issued.Raw); !ok {
		t.Fatal("expected cached validate to succeed")
	}

	// ── Use the key through the AuthMiddleware wired to this store ─
	mw := auth.NewAuthMiddleware(ks)
	handler := mw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-ID") != tenantID {
			http.Error(w, "tenant mismatch", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth middleware: expected 200, got %d", rec.Code)
	}

	// ── Revoke ──────────────────────────────────────────────────────
	if err := ks.RevokeKey(ctx, tenantID, issued.KID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Wait for cache TTL (100ms) to expire so the SELECT runs again.
	time.Sleep(250 * time.Millisecond)

	if _, ok := ks.ValidateKey(issued.Raw); ok {
		t.Fatal("expected revoked key to no longer validate")
	}

	// ── Re-revoke must report already-revoked, not a hard error ─────
	if err := ks.RevokeKey(ctx, tenantID, issued.KID); err != auth.ErrKeyRevoked {
		t.Fatalf("re-revoke: want ErrKeyRevoked, got %v", err)
	}

	// ── Spoofed tenant prefix on a real kid must fail ───────────────
	parts := strings.SplitN(issued.Raw, ":", 3)
	spoofed := "evil:" + parts[1] + ":" + parts[2]
	if _, ok := ks.ValidateKey(spoofed); ok {
		t.Fatal("expected spoofed tenant id to be rejected")
	}
}
