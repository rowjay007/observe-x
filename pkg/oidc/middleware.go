package oidc

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ─── Stdlib middleware ───────────────────────────────────────────────────

type ctxClaimsKey struct{}

// WithClaims attaches the validated claims to ctx.
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxClaimsKey{}, c)
}

// ClaimsFromContext returns the claims attached by the middleware
// (zero value if absent).
func ClaimsFromContext(ctx context.Context) Claims {
	v, _ := ctx.Value(ctxClaimsKey{}).(Claims)
	return v
}

// Middleware turns the validator into an http.Handler wrapper.
// On success it forwards with the claims in ctx and the principal's
// subject in the X-Operator-Subject header (useful for audit logs).
func (v *Validator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerFromHeader(r.Header.Get("Authorization"))
		if token == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		claims, err := v.Validate(r.Context(), token)
		if err != nil {
			if IsAuthz(err) {
				http.Error(w, "operator not in admin group", http.StatusForbidden)
				return
			}
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-Operator-Subject", claims.Subject)
		if claims.Email != "" {
			r.Header.Set("X-Operator-Email", claims.Email)
		}
		r = r.WithContext(WithClaims(r.Context(), claims))
		next.ServeHTTP(w, r)
	})
}

// ─── Gin adapter ─────────────────────────────────────────────────────────

// Gin wraps the stdlib middleware so it composes with gin routes the
// same way the auth package does (see services/*/cmd/main.go ginAuth).
func (v *Validator) Gin() gin.HandlerFunc {
	return func(c *gin.Context) {
		v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
		if c.Writer.Written() {
			c.Abort()
		}
	}
}

func bearerFromHeader(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
