# ADR-0014 — Operator authentication via OIDC

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3b

## Context

Phase B-1 (ADR-0004) introduced `tenant-api` with a single static
**bootstrap admin token** in front of every admin endpoint. That
token had to live in a Kubernetes Secret, was the same for every
operator, and had no audit attribution beyond a hard-coded "admin"
actor string. It was always meant to be a Phase B placeholder — the
roadmap and ADR explicitly called it out as "replaced by OIDC in a
later phase."

A SOC2 / ISO 27001 / FedRAMP-style production deployment requires:

1. **Per-operator identity** — every audit row must name a human
   (or a service principal with a stable identifier), not "admin."
2. **No long-lived shared secrets** — tokens must be short-lived and
   issued by an authority the org already trusts (Google, Okta,
   Keycloak, Azure AD, Auth0, etc.).
3. **Group/role-based authorisation** — not every authenticated
   operator is an admin; the IdP's groups claim drives access.
4. **Standard tooling** — `kubectl` users, GitHub OIDC for CI, and
   browser SSO must all work without ObserveX-specific glue.

OIDC is the universal answer. We do NOT want to run an OIDC issuer;
we want to **validate** OIDC bearer tokens at the API boundary.

## Decision

Introduce `pkg/oidc` as a thin OIDC bearer-token validator:

- **Discovery** at `{Issuer}/.well-known/openid-configuration`
  with constant-time issuer cross-check (rejects an impersonating
  issuer that returns a different `iss` in its discovery doc).
- **JWKS** auto-refresh on a ticker (default 15m) and on `kid` miss
  (handles fresh rotation without waiting for the ticker).
- **Signature** validation accepting the OIDC-required algorithms
  only: RS{256,384,512}, ES{256,384,512}, EdDSA. (No HS256 — a
  shared secret defeats the purpose of an asymmetric IdP.)
- **`iss`, `aud`, `exp`, `nbf`** validation with a 60s skew window
  (configurable).
- **Group claim** evaluation: optional `AdminGroups` allowlist
  resolved via `cfg.GroupClaim` (default `groups`, overridable for
  IdPs that use namespaced claims like
  `https://my-org/roles`). Tokens whose groups don't intersect the
  allowlist get 403 with a distinct sentinel error so the middleware
  can distinguish "bad token" from "good token, wrong group."
- **Generic error surface** to the client. Internally we
  distinguish `IsAuthn` (401) from `IsAuthz` (403); externally the
  HTTP body never leaks the underlying cause.

`tenant-api` wires the validator behind `requireAdmin()`. The OIDC
validator's middleware sets `X-Operator-Subject` (and
`X-Operator-Email` when present); the existing `audit()` helper
upgrades the actor field from the static "admin" to the operator's
subject.

The static admin-token survives as a **break-glass**: if
`OBSERVE_X_OIDC_ISSUER` is unset, the bootstrap token path is the
only auth. If both are configured, the service fails to start —
dual-path auth is a confused-deputy invitation.

## Trade-offs

- **No OAuth2 flow.** ObserveX validates tokens; it does not issue
  them. The browser SSO flow, kubectl device code, GitHub OIDC, or a
  client-credentials grant all live outside ObserveX. That keeps the
  surface area honest and avoids re-implementing an IdP poorly.

- **No SCIM / user-DB sync.** Group membership lives in the IdP and
  is presented on every token. We don't cache it server-side. If
  group membership changes during a token's lifetime, the change
  takes effect on the next token refresh, not immediately. For most
  IdPs this is a 5–60 minute window. Acceptable for ObserveX's
  operator surface; not for the data plane (which uses opaque
  tenant API keys with explicit revocation per ADR-0011).

- **Skew window of 60s.** Standard practice; configurable. Larger
  windows ease IdP/issuer clock drift but widen the replay window
  for compromised tokens. 60s is the OIDC RP "reasonable default."

- **HS256 deliberately rejected.** Symmetric signing requires the
  validator to hold the issuer's signing secret, defeating the
  no-shared-secret guarantee.

- **Break-glass kept on purpose.** A real-world incident where the
  IdP is unreachable (DNS, outage, misconfigured cert) must not
  also lock operators out of the control plane. The break-glass is
  a documented, intentionally narrow fallback — enabled only when
  no OIDC issuer is configured, never simultaneously with one.

## Package changes

- `pkg/oidc/oidc.go` (new): `Validator`, `Config`, `Claims`,
  `IsAuthn`, `IsAuthz`, discovery + JWKS refresh internals.
- `pkg/oidc/middleware.go` (new): stdlib + gin middleware adapters,
  `WithClaims` / `ClaimsFromContext`, `X-Operator-Subject` /
  `-Email` propagation.
- `pkg/oidc/oidc_test.go` (new): in-process IdP using
  `httptest.Server` + RSA keypair; tests cover happy path, wrong
  audience, expired token, insufficient group, custom group claim,
  middleware 401, subject propagation.
- `services/tenant-api/cmd/main.go` (modified): startup wires the
  validator iff `OBSERVE_X_OIDC_ISSUER` is set; refuses both auth
  modes simultaneously; `requireAdmin()` returns the validator's gin
  adapter in OIDC mode; `audit()` records the validated subject.

New direct dependency: `github.com/go-jose/go-jose/v4` (already a
transitive dep via the AWS SDK; promoted to a direct import here).
Pure Go, no CGo, ~120 KB add-on to the linked binaries.

## Environment

```
OBSERVE_X_OIDC_ISSUER          required to enable OIDC; e.g. https://login.example.com
OBSERVE_X_OIDC_AUDIENCE        defaults to "observex"; must match the JWT's aud
OBSERVE_X_OIDC_ADMIN_GROUPS    comma-separated allowlist; empty ⇒ any authenticated principal is admin
OBSERVE_X_OIDC_GROUP_CLAIM     JSON key for the groups list; default "groups"
OBSERVE_X_TENANT_API_ADMIN_TOKEN  break-glass fallback; mutually exclusive with OIDC_ISSUER
```

## Alternatives considered

- **mTLS for operators.** Works but couples operator identity to
  PKI tooling we'd then have to ship; less universal than OIDC.

- **Static JWT signing key inside ObserveX.** Same problem as
  HS256 — moves the trust root into ObserveX rather than the IdP.

- **OAuth2-Proxy sidecar.** Externalises everything but adds a
  hop, complicates the deployment, and gives us no programmatic
  access to the claims for audit attribution.

- **Casbin / Cerbos for fine-grained policy.** Premature for the
  admin surface, which is binary (admin or not). The data plane's
  finer-grained policy lives in `pkg/auth` scopes (ADR-0011).

## Verification

- `go test -race ./pkg/oidc/...`: validates discovery, JWKS rotation
  via in-process IdP, signature validation across RS256, all the
  `iss`/`aud`/`exp` rejection paths, group allowlist, custom group
  claim, middleware behaviour.
- `tenant-api` build + smoke-test in CI now exercises both OIDC and
  break-glass paths (helm chart values demonstrate the OIDC config
  block).

## Follow-ups

- **Operator audit aggregation in alert-manager** still uses the
  generic actor string. Once Phase C-4 ships the UI server, the UI
  will surface the validated subject in alert notifications.

- **Client-side OIDC helper.** The CLI / UI need a small helper to
  perform the device-code flow against the configured issuer and
  cache the token. Deferred to Phase C-4 (UI). For now, operators
  use any standard OIDC client (kubectl, oidc-login,
  Postman-with-OIDC-plugin, etc.).
