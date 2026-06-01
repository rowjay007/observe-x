// Scopes — least-privilege bearer-token authorisation for ObserveX.
//
// Phase C-3a introduces explicit per-key scopes so a key issued for a
// CI runner that only ingests metrics cannot be used to query, alert,
// or rotate keys. Without scopes, any leaked key has full tenant
// access — a CVE-class concern for a multi-tenant platform.
//
// The four canonical scopes are:
//
//	ingest         — write signals to ingest-gateway /v1/{ingest,traces,metrics,logs}
//	query          — read via query-engine /v1/query
//	alert.read     — list alerts, silences, SLOs at alert-manager
//	alert.write    — create silences, fire events, register SLOs
//	tenant.admin   — operator-level (issue/revoke keys, mutate tenant)
//
// Scopes compose: a key with [ingest, query] can do both. A key with
// just [ingest] receives 403 on /v1/query, with WWW-Authenticate
// describing the required scope.
//
// Wire shape on KeyMetadata.Scopes is the canonical lowercase string
// ("ingest", not "INGEST"). The Postgres column is TEXT[] and the
// store normalises on read.
package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// Scope is the canonical privilege string.
type Scope string

const (
	ScopeIngest      Scope = "ingest"
	ScopeQuery       Scope = "query"
	ScopeAlertRead   Scope = "alert.read"
	ScopeAlertWrite  Scope = "alert.write"
	ScopeTenantAdmin Scope = "tenant.admin"
)

// AllScopes is the canonical set returned to operators that want a
// max-privilege key. Order is sorted/stable so tests don't churn.
func AllScopes() []Scope {
	return []Scope{
		ScopeIngest, ScopeQuery,
		ScopeAlertRead, ScopeAlertWrite,
		ScopeTenantAdmin,
	}
}

// DefaultScopes is what tenant-api hands out when a key is requested
// without an explicit scope list. We DO NOT default to AllScopes
// because that defeats the purpose of scopes; we default to the
// least-surprising historical scope (ingest), matching the SQL
// column default.
func DefaultScopes() []Scope { return []Scope{ScopeIngest} }

// ParseScope returns ok=false for unknown strings. Empty input ⇒ false.
func ParseScope(s string) (Scope, bool) {
	switch Scope(strings.ToLower(strings.TrimSpace(s))) {
	case ScopeIngest:
		return ScopeIngest, true
	case ScopeQuery:
		return ScopeQuery, true
	case ScopeAlertRead:
		return ScopeAlertRead, true
	case ScopeAlertWrite:
		return ScopeAlertWrite, true
	case ScopeTenantAdmin:
		return ScopeTenantAdmin, true
	}
	return "", false
}

// ParseScopes batches ParseScope and returns the first unknown scope
// as an error so the caller can surface a useful 400.
func ParseScopes(in []string) ([]Scope, error) {
	if len(in) == 0 {
		return DefaultScopes(), nil
	}
	out := make([]Scope, 0, len(in))
	for _, s := range in {
		sc, ok := ParseScope(s)
		if !ok {
			return nil, errors.New("auth: unknown scope " + s)
		}
		out = append(out, sc)
	}
	return dedupSortScopes(out), nil
}

// ScopesAsStrings is the inverse: canonical string slice for SQL/JSON.
func ScopesAsStrings(in []Scope) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// HasScope reports whether `granted` contains every scope in `required`.
// A key is authorised iff it holds ALL the required scopes — there is
// no OR semantic. Each endpoint declares exactly one required scope
// today; the AND shape is forward-compatible for endpoints that need
// (e.g.) ingest + alert.write together later.
func HasScope(granted []Scope, required ...Scope) bool {
	if len(required) == 0 {
		return true
	}
	set := make(map[Scope]struct{}, len(granted))
	for _, g := range granted {
		set[g] = struct{}{}
	}
	for _, r := range required {
		if _, ok := set[r]; !ok {
			return false
		}
	}
	return true
}

func dedupSortScopes(in []Scope) []Scope {
	seen := make(map[Scope]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ─── Context plumbing ────────────────────────────────────────────────────

type ctxScopesKey struct{}

// WithScopes attaches the granted scope set to ctx so handlers can
// re-check beyond what the middleware enforced.
func WithScopes(ctx context.Context, scopes []Scope) context.Context {
	return context.WithValue(ctx, ctxScopesKey{}, scopes)
}

// ScopesFromContext returns the scope set attached by the middleware,
// or DefaultScopes() if missing (defensive default — handlers should
// not rely on this branch in production).
func ScopesFromContext(ctx context.Context) []Scope {
	v, _ := ctx.Value(ctxScopesKey{}).([]Scope)
	if len(v) == 0 {
		return DefaultScopes()
	}
	return v
}

// ─── KeyMetadata: returned by KeyStores that support scopes ──────────────

// KeyMetadata is the richer return shape for scope-aware stores. The
// historical ValidateKey path returns (tenantID, ok); scope-aware
// callers should prefer ValidateKeyWithMetadata, which returns the
// scope set as well.
type KeyMetadata struct {
	TenantID string
	Scopes   []Scope
}

// ScopeAwareKeyStore is the optional extension that PostgresKeyStore
// and tests implement. KeyStores that don't ⇒ middleware treats them
// as having DefaultScopes() (which only authorises /v1/ingest).
type ScopeAwareKeyStore interface {
	KeyStore
	ValidateKeyWithMetadata(key string) (KeyMetadata, bool)
}

// ─── Scope-aware middleware helper ───────────────────────────────────────

// RequireScope produces an http.Handler middleware that 403s any
// request whose validated key lacks every scope in required. Mount it
// AFTER AuthMiddleware (which sets X-Tenant-ID + the context scope
// set).
//
// Returning a stdlib middleware (not gin.HandlerFunc) keeps this
// usable from any router; the gin wrappers already handle the
// adapter pattern.
func (m *AuthMiddleware) RequireScope(required ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			granted := ScopesFromContext(r.Context())
			if !HasScope(granted, required...) {
				w.Header().Set("WWW-Authenticate",
					"Bearer scope=\""+strings.Join(scopeList(required), " ")+"\"")
				http.Error(w, "insufficient scope", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func scopeList(in []Scope) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}
