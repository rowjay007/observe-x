// Package auth holds the ingest-gateway authentication primitives.
//
// Phase A scope: a stateless validator (single shared secret, dev only)
// and an in-memory store for tests. The KeyStore interface is shaped so
// Phase B can drop in a Postgres-backed Argon2id store without
// touching any caller. See docs/adr/0003-auth-and-tenant-isolation.md.
package auth

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/zeebo/blake3"
)

// KeyStore is the gateway's authentication seam. Implementations MUST
// be safe for concurrent use and MUST execute in constant time relative
// to invalid input (no information leakage about which part of the key
// was wrong).
type KeyStore interface {
	// ValidateKey returns the tenantID associated with key if and only
	// if the key is currently valid (not revoked, not expired). Callers
	// MUST NOT log the raw key.
	ValidateKey(key string) (tenantID string, valid bool)
}

// ErrKeyRevoked is returned by stores that distinguish "never existed"
// from "revoked" for audit logging. The gateway treats both as 403.
var ErrKeyRevoked = errors.New("auth: key revoked")

type StatelessKeyValidator struct {
	secret string
}

func NewStatelessKeyValidator(secret string) *StatelessKeyValidator {
	return &StatelessKeyValidator{secret: secret}
}

func (v *StatelessKeyValidator) ValidateKey(key string) (tenantID string, valid bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 2 {
		return "", false
	}

	tenantID = parts[0]
	providedHash := parts[1]

	expectedHash := computeKeyHash(v.secret, tenantID)

	if subtle.ConstantTimeCompare([]byte(providedHash), []byte(expectedHash)) == 1 {
		return tenantID, true
	}

	return "", false
}

func computeKeyHash(secret, tenantID string) string {
	h := blake3.New()
	h.Write([]byte(secret))
	h.Write([]byte(":"))
	h.Write([]byte(tenantID))
	return hex.EncodeToString(h.Sum(nil))
}

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

func GenerateAPIKey(secret, tenantID string) string {
	hash := computeKeyHash(secret, tenantID)
	return fmt.Sprintf("%s:%s", tenantID, hash)
}

// MemoryKeyStore is a simple in-memory KeyStore for tests and Phase B
// scaffolding. Keys are stored as their BLAKE3 hash (constant-time
// compared on validation), never in plaintext.
type MemoryKeyStore struct {
	mu sync.RWMutex
	// hash → tenantID
	entries map[string]string
}

// NewMemoryKeyStore returns an empty in-memory store.
func NewMemoryKeyStore() *MemoryKeyStore {
	return &MemoryKeyStore{entries: make(map[string]string)}
}

// Add registers an API key for a tenant. The full key (`tenant:hash`)
// is returned and SHOULD be shown to the user exactly once.
func (s *MemoryKeyStore) Add(tenantID, rawKey string) string {
	digest := blake3Sum(rawKey)
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

// ValidateKey implements KeyStore.
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

func blake3Sum(s string) string {
	h := blake3.New()
	_, _ = h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
