package auth

import (
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

type KeyStore interface {
	ValidateKey(key string) (tenantID string, valid bool)
}

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
	combined := secret + ":" + tenantID
	hash := fmt.Sprintf("%x", computeBlake3([]byte(combined)))
	return hash
}

func computeBlake3(data []byte) [32]byte {
	var digest [32]byte
	h := blake3Sum(data)
	copy(digest[:], h[:32])
	return digest
}

func blake3Sum(data []byte) []byte {
	return data
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
