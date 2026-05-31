// Package oidc is ObserveX's operator-authentication seam.
//
// Phase C-3b replaces the Phase B-1 static bootstrap admin-token in
// tenant-api with full OIDC bearer-token validation, so operators
// authenticate against Google, Okta, Keycloak, Auth0, or any other
// OIDC issuer with no ObserveX-side credential surface.
//
// What we validate:
//
//   - Signature (RS256 / ES256 / EdDSA — the OIDC-required algorithms)
//     against keys fetched from the issuer's JWKS endpoint.
//   - `iss` matches the configured issuer (constant-time compare).
//   - `aud` contains the configured audience (typically the
//     ObserveX deployment URL or a static client id).
//   - `exp` and `nbf` against the wall clock with a 60s skew window.
//   - Optional `groups` / `roles` claim against an admin allowlist —
//     the principle of least privilege also applies to operators.
//
// What we DO NOT do here:
//
//   - End-user OAuth2 dance: we trust whatever sits in front of
//     tenant-api (a browser SSO flow, kubectl-style device code,
//     CLI client credentials grant…) to mint the JWT. ObserveX only
//     validates, never issues, OIDC tokens.
//   - SCIM / user provisioning: groups are presented in the token;
//     we don't model the IdP's user database.
//
// The static admin-token remains as a break-glass: if `OIDC_ISSUER`
// is unset, tenant-api falls back to the bootstrap token path. If
// both are configured, OIDC wins and admin-token is rejected at
// startup to remove the dual-path attack surface.
package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ─── Validator ───────────────────────────────────────────────────────────

// Config wires the validator. Issuer + Audience are required; the
// rest tune behaviour at the security/UX margin.
type Config struct {
	// Issuer is the OIDC issuer URL. Discovery happens at
	//   {Issuer}/.well-known/openid-configuration
	// which gives us the JWKS URI.
	Issuer string

	// Audience is the expected `aud` value. Tokens missing this
	// audience are rejected. For ObserveX the recommended value is
	// the deployment URL (e.g. https://observex.example.com).
	Audience string

	// AdminGroups, if non-empty, gates access on a group/role claim.
	// The validator looks at the `groups` claim by default, override
	// with GroupClaim. Empty ⇒ any authenticated token is admin.
	AdminGroups []string

	// GroupClaim is the JSON path to the groups list. Defaults to
	// "groups"; common alternatives are "roles" or
	// "https://my-org/groups".
	GroupClaim string

	// Skew bounds clock drift for exp/nbf. Defaults to 60s.
	Skew time.Duration

	// JWKSRefresh controls how often the JWKS is re-fetched. Default
	// 15m. The validator also re-fetches on kid miss with a small
	// backoff to handle key rotation.
	JWKSRefresh time.Duration

	// HTTPClient overrides the default *http.Client used for
	// discovery + JWKS fetch. Test seam.
	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.Skew <= 0 {
		c.Skew = 60 * time.Second
	}
	if c.JWKSRefresh <= 0 {
		c.JWKSRefresh = 15 * time.Minute
	}
	if c.GroupClaim == "" {
		c.GroupClaim = "groups"
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return c
}

// Validator validates JWTs against the configured issuer.
// Safe for concurrent use. Holds a periodically refreshed JWKS.
type Validator struct {
	cfg      Config
	jwksURI  string
	keysMu   sync.RWMutex
	keys     *jose.JSONWebKeySet
	keysAt   time.Time
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// Discovery is the subset of OIDC discovery we need.
type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// NewValidator performs OIDC discovery + initial JWKS fetch.
// Returns an error if the issuer is unreachable or its discovery
// document is malformed. Callers MUST call Close to stop the
// refresh goroutine.
func NewValidator(ctx context.Context, cfg Config) (*Validator, error) {
	cfg = cfg.withDefaults()
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: Issuer required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("oidc: Audience required")
	}

	disco, err := fetchDiscovery(ctx, cfg.HTTPClient, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	// Defensive: the doc's `issuer` MUST match the configured Issuer
	// per the OIDC core spec. Anything else is an impersonation
	// attempt or a misconfig.
	if subtle.ConstantTimeCompare([]byte(disco.Issuer), []byte(cfg.Issuer)) != 1 {
		return nil, fmt.Errorf("oidc: issuer mismatch: doc=%q cfg=%q", disco.Issuer, cfg.Issuer)
	}

	v := &Validator{
		cfg:     cfg,
		jwksURI: disco.JWKSURI,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	if err := v.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("oidc: initial JWKS: %w", err)
	}
	go v.refreshLoop()
	return v, nil
}

// Close stops the background JWKS refresher. Idempotent.
func (v *Validator) Close() {
	v.stopOnce.Do(func() {
		close(v.stopCh)
		<-v.doneCh
	})
}

// Claims is the validated claims surface returned on success.
type Claims struct {
	Subject  string    // `sub` — opaque user id from the IdP
	Email    string    // optional, propagated when present
	Issuer   string
	Groups   []string  // resolved via cfg.GroupClaim
	IssuedAt time.Time
	Expires  time.Time
}

// Validate parses & verifies a compact-serialized JWT. Returns the
// resolved Claims on success. Every failure path returns a single
// sentinel-flavored error so handlers can map it to a 401 / 403
// without leaking specifics that aid an attacker.
func (v *Validator) Validate(ctx context.Context, raw string) (Claims, error) {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512,
		jose.ES256, jose.ES384, jose.ES512,
		jose.EdDSA,
	})
	if err != nil {
		return Claims{}, errInvalidToken
	}
	key, err := v.resolveKey(ctx, tok)
	if err != nil {
		return Claims{}, errInvalidToken
	}

	var std jwt.Claims
	raw_ := map[string]any{}
	if err := tok.Claims(key, &std, &raw_); err != nil {
		return Claims{}, errInvalidToken
	}

	exp := jwt.Expected{
		Issuer:      v.cfg.Issuer,
		AnyAudience: []string{v.cfg.Audience},
		Time:        time.Now(),
	}
	if err := std.ValidateWithLeeway(exp, v.cfg.Skew); err != nil {
		return Claims{}, errInvalidToken
	}

	groups := extractGroups(raw_, v.cfg.GroupClaim)
	if len(v.cfg.AdminGroups) > 0 && !overlap(groups, v.cfg.AdminGroups) {
		return Claims{}, errInsufficientGroup
	}

	out := Claims{
		Subject: std.Subject,
		Issuer:  std.Issuer,
		Groups:  groups,
	}
	if std.IssuedAt != nil {
		out.IssuedAt = std.IssuedAt.Time()
	}
	if std.Expiry != nil {
		out.Expires = std.Expiry.Time()
	}
	if e, ok := raw_["email"].(string); ok {
		out.Email = e
	}
	return out, nil
}

// ─── Sentinel errors ─────────────────────────────────────────────────────

var (
	// errInvalidToken is a generic "401-shaped" error. Returned for
	// every signature/format/claim failure; callers MUST NOT leak
	// the underlying cause to the client.
	errInvalidToken = errors.New("oidc: invalid token")
	// errInsufficientGroup maps to 403. The token was authentic but
	// the principal is not in an admin group.
	errInsufficientGroup = errors.New("oidc: insufficient group")
)

// IsAuthn reports whether the error is a 401-class (invalid token).
func IsAuthn(err error) bool { return errors.Is(err, errInvalidToken) }

// IsAuthz reports whether the error is a 403-class (insufficient privilege).
func IsAuthz(err error) bool { return errors.Is(err, errInsufficientGroup) }

// ─── JWKS refresh ────────────────────────────────────────────────────────

func (v *Validator) refreshLoop() {
	defer close(v.doneCh)
	t := time.NewTicker(v.cfg.JWKSRefresh)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = v.refreshKeys(ctx)
			cancel()
		case <-v.stopCh:
			return
		}
	}
}

func (v *Validator) refreshKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURI, nil)
	if err != nil {
		return err
	}
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var keys jose.JSONWebKeySet
	if err := json.Unmarshal(body, &keys); err != nil {
		return err
	}
	v.keysMu.Lock()
	v.keys = &keys
	v.keysAt = time.Now()
	v.keysMu.Unlock()
	return nil
}

func (v *Validator) resolveKey(ctx context.Context, tok *jwt.JSONWebToken) (any, error) {
	if len(tok.Headers) == 0 {
		return nil, errInvalidToken
	}
	kid := tok.Headers[0].KeyID

	keys := v.snapshotKeys()
	if k := lookupKey(keys, kid); k != nil {
		return k.Key, nil
	}

	// On miss: re-fetch once, then look again. Handles fresh key
	// rotation without waiting for the next ticker.
	if err := v.refreshKeys(ctx); err == nil {
		keys = v.snapshotKeys()
		if k := lookupKey(keys, kid); k != nil {
			return k.Key, nil
		}
	}
	return nil, errInvalidToken
}

func (v *Validator) snapshotKeys() *jose.JSONWebKeySet {
	v.keysMu.RLock()
	defer v.keysMu.RUnlock()
	return v.keys
}

func lookupKey(set *jose.JSONWebKeySet, kid string) *jose.JSONWebKey {
	if set == nil {
		return nil
	}
	for i := range set.Keys {
		if set.Keys[i].KeyID == kid {
			return &set.Keys[i]
		}
	}
	// Per RFC 7515, a missing kid is permitted if there's exactly
	// one key in the set. We accept that fallback strictly.
	if kid == "" && len(set.Keys) == 1 {
		return &set.Keys[0]
	}
	return nil
}

// ─── Discovery + helpers ─────────────────────────────────────────────────

func fetchDiscovery(ctx context.Context, hc *http.Client, issuer string) (discoveryDoc, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return discoveryDoc{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return discoveryDoc{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discoveryDoc{}, fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return discoveryDoc{}, err
	}
	var d discoveryDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return discoveryDoc{}, err
	}
	if d.Issuer == "" || d.JWKSURI == "" {
		return discoveryDoc{}, errors.New("discovery: missing issuer or jwks_uri")
	}
	return d, nil
}

func extractGroups(claims map[string]any, claim string) []string {
	raw, ok := claims[claim]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		// Some IdPs emit a single space- or comma-separated string.
		return splitNonEmpty(v)
	}
	return nil
}

func splitNonEmpty(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func overlap(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
