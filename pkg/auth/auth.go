// Package auth holds the ObserveX authentication primitives shared by
// every service that needs to validate a caller's identity.
//
// Phase B-1 makes this the single canonical home for:
//
//   - The KeyStore seam (interface implementations are dev-only
//     StatelessKeyValidator, in-memory MemoryKeyStore, and the
//     production PostgresKeyStore — see store_postgres.go).
//   - The TLSConfig loader for server-side TLS / mTLS.
//   - The HTTP AuthMiddleware that adapts any KeyStore into a stdlib
//     http.Handler.
//   - GenerateAPIKey + BLAKE3 helpers, used both at issuance time and
//     in tests.
//
// See docs/adr/0003-auth-and-tenant-isolation.md and
// docs/adr/0004-tenant-control-plane.md for the rationale.
package auth

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/zeebo/blake3"
)

// ─── KeyStore seam ────────────────────────────────────────────────────────

// KeyStore is the authentication seam consumed by every receiver.
// Implementations MUST be safe for concurrent use and MUST execute
// in constant time relative to invalid input.
type KeyStore interface {
	// ValidateKey returns the tenantID associated with key if and only
	// if the key is currently valid (not revoked, not expired).
	// Callers MUST NOT log the raw key.
	ValidateKey(key string) (tenantID string, valid bool)
}

// ErrKeyRevoked is returned by stores that distinguish "never existed"
// from "revoked" for audit logging. The gateway treats both as 403.
var ErrKeyRevoked = errors.New("auth: key revoked")

// ─── Shared helpers ───────────────────────────────────────────────────────

// blake3Sum returns the lowercase hex BLAKE3 digest of s. Exported only
// so the tenant-api and tests can hash key fragments consistently.
func Blake3Sum(s string) string {
	h := blake3.New()
	_, _ = h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// ─── StatelessKeyValidator (dev only) ─────────────────────────────────────

// StatelessKeyValidator derives every key from a single shared secret.
// This is intentionally dev-only: any leak of the secret lets an
// attacker mint a valid key for every tenant. The README and ADR-0003
// document this explicitly.
type StatelessKeyValidator struct {
	secret string
}

func NewStatelessKeyValidator(secret string) *StatelessKeyValidator {
	return &StatelessKeyValidator{secret: secret}
}

func (v *StatelessKeyValidator) ValidateKey(key string) (string, bool) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	tenantID := parts[0]
	providedHash := parts[1]
	expected := computeKeyHash(v.secret, tenantID)
	if subtle.ConstantTimeCompare([]byte(providedHash), []byte(expected)) == 1 {
		return tenantID, true
	}
	return "", false
}

// GenerateAPIKey returns the dev-mode "tenant:hash(secret||':'||tenant)"
// key. Production code uses PostgresKeyStore.IssueKey instead.
func GenerateAPIKey(secret, tenantID string) string {
	return fmt.Sprintf("%s:%s", tenantID, computeKeyHash(secret, tenantID))
}

func computeKeyHash(secret, tenantID string) string {
	h := blake3.New()
	h.Write([]byte(secret))
	h.Write([]byte(":"))
	h.Write([]byte(tenantID))
	return hex.EncodeToString(h.Sum(nil))
}

// ─── MemoryKeyStore ───────────────────────────────────────────────────────

// MemoryKeyStore is the in-memory implementation used by tests and as
// Phase B-1 scaffolding. Keys are stored as BLAKE3 digests (constant-
// time compared on validation), never in plaintext.
type MemoryKeyStore struct {
	mu      sync.RWMutex
	entries map[string]string // hash → tenantID
}

func NewMemoryKeyStore() *MemoryKeyStore {
	return &MemoryKeyStore{entries: make(map[string]string)}
}

// Add registers a tenant:hash mapping derived from rawKey. Returns the
// full wire-format key (`tenant:digest`) which SHOULD be shown to the
// user exactly once.
func (s *MemoryKeyStore) Add(tenantID, rawKey string) string {
	digest := Blake3Sum(rawKey)
	full := fmt.Sprintf("%s:%s", tenantID, digest)
	s.mu.Lock()
	s.entries[digest] = tenantID
	s.mu.Unlock()
	return full
}

// Revoke removes a key. Idempotent.
func (s *MemoryKeyStore) Revoke(key string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return
	}
	s.mu.Lock()
	delete(s.entries, parts[1])
	s.mu.Unlock()
}

func (s *MemoryKeyStore) ValidateKey(key string) (string, bool) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	s.mu.RLock()
	tenantID, ok := s.entries[parts[1]]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(tenantID)) != 1 {
		return "", false
	}
	return tenantID, true
}

// ─── HTTP middleware ──────────────────────────────────────────────────────

// AuthMiddleware enforces the bearer-token contract over an arbitrary
// KeyStore. On success it sets the X-Tenant-ID request header so
// downstream handlers don't need to re-parse the key.
type AuthMiddleware struct {
	keyStore KeyStore
}

func NewAuthMiddleware(keyStore KeyStore) *AuthMiddleware {
	return &AuthMiddleware{keyStore: keyStore}
}

func (m *AuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "missing authorization header", http.StatusUnauthorized)
			return
		}
		const bearerScheme = "Bearer "
		if !strings.HasPrefix(authHeader, bearerScheme) {
			http.Error(w, "invalid authorization scheme", http.StatusUnauthorized)
			return
		}
		key := authHeader[len(bearerScheme):]
		tenantID, valid := m.keyStore.ValidateKey(key)
		if !valid {
			http.Error(w, "invalid api key", http.StatusForbidden)
			return
		}
		r.Header.Set("X-Tenant-ID", tenantID)
		next.ServeHTTP(w, r)
	})
}

// ─── TLS / mTLS configuration ─────────────────────────────────────────────

// TLSConfig describes the server-side certificate material. When CAFile
// is set, mTLS is required and the client cert MUST chain to that CA.
type TLSConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

func (cfg *TLSConfig) LoadServerConfig() (*tls.Config, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("auth: cert and key files required for TLS")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("auth: load server certs: %w", err)
	}
	out := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if cfg.CAFile != "" {
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("auth: parse CA")
		}
		out.ClientCAs = pool
		out.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return out, nil
}

func (cfg *TLSConfig) LoadClientConfig() (*tls.Config, error) {
	out := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("auth: load client certs: %w", err)
		}
		out.Certificates = []tls.Certificate{cert}
	}
	if cfg.CAFile != "" {
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("auth: parse CA")
		}
		out.RootCAs = pool
	}
	return out, nil
}
