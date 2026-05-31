# ADR-0011 — API key scopes (least-privilege bearer tokens)

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3a

## Context

Phase B-1 introduced a Postgres-backed bearer-token auth system
(`pkg/auth.PostgresKeyStore`, ADR-0003 + ADR-0004). It gives us
revocation, rotation, last-used tracking, and Argon2id hashing — but
every issued key has the same blast radius: full ingest + full query
+ full alert mutation + tenant admin. A single leaked key from a CI
runner could be used to drain the data plane, mutate the SLO catalog,
or rotate every other key.

This is a CVE-class concern in any multi-tenant platform: SOC2
expects the principle of least privilege; any audit will flag the
"one shape fits all" design.

## Decision

Introduce explicit per-key **scopes** with five canonical values and
enforce them at the middleware boundary in every service:

| Scope          | Granted to                              | Endpoints                              |
|----------------|-----------------------------------------|----------------------------------------|
| `ingest`       | SDK runners, CI agents                  | `POST /v1/ingest`, `POST /v1/traces`, `POST /v1/metrics`, `POST /v1/logs`, gRPC OTLP services |
| `query`        | dashboards, ad-hoc analyst keys         | `POST /v1/query`                       |
| `alert.read`   | UI, on-call console                     | `GET /v1/alerts`                       |
| `alert.write`  | incident-response automations           | `POST /v1/events`, `POST /v1/observations`, `POST /v1/silences` |
| `tenant.admin` | platform operators                      | `POST /v1/slos` (and anything that mutates the SLO catalog) |

The wire format and DB shape are unchanged: scopes ride alongside the
existing `tenant_api_keys` row via a `scopes TEXT[]` column (already
present in the Phase B-1 schema). `IssueKeyWithScopes` accepts an
explicit list at creation time; `IssueKey` (legacy) defaults to
`DefaultScopes() = [ingest]` for backwards compatibility.

The hot path is unchanged in shape: `ValidateKeyWithMetadata` returns
the scope set, the validation cache stores it (zero extra DB load),
and `auth.GinRequireScope(...)` is the per-route guard. Tenant id and
scope set are propagated through the request context.

## Trade-offs

- **AND semantic, not OR.** `RequireScope(a, b)` means "must hold
  both." We chose AND because the only realistic policy is "this
  endpoint requires exactly this capability" and OR creates surprise
  ("why did my read key suddenly write?"). Forward-compatible for
  composite requirements like `(ingest AND alert.write)`.

- **Dev-mode `StatelessKeyValidator` grants all scopes.** This is
  intentional — it's a single-secret dev-only validator and the
  threat model already says "if the secret leaks, every tenant is
  compromised." Granting all scopes preserves that behavior and
  avoids accidental dev/prod drift in tests. ADR-0003 already says
  PostgresKeyStore is required for production.

- **No OR-of-scopes today.** If we need it later, `HasScope`
  becomes the wrong primitive; we'd introduce `HasAnyScope`. Left out
  to keep the policy mental model simple.

- **Cache poisoning safety.** The cache key is `BLAKE3(full wire
  key)`; revocation evicts via TTL (default 5s). Scope changes on a
  rotated key take effect within the cache TTL. We did NOT add
  scope-change-on-existing-kid because we don't support mutating a
  key's scopes — operators must issue a new key. This avoids the
  classic "I revoked X but a stale cached lookup still has Y scopes"
  bug.

- **WWW-Authenticate.** 403 responses include
  `WWW-Authenticate: Bearer scope="..."` so clients can surface a
  precise error: "this key is missing the `query` scope" rather than
  a generic 403.

## Package changes

- `pkg/auth/scopes.go` (new): `Scope` type, `AllScopes`,
  `DefaultScopes`, `ParseScopes`, `HasScope`,
  `KeyMetadata`, `ScopeAwareKeyStore` interface, `WithScopes` /
  `ScopesFromContext`, `RequireScope` (stdlib middleware).
- `pkg/auth/gin.go` (new): `GinRequireScope` for the gin services.
- `pkg/auth/store_postgres.go` (modified):
  `ValidateKeyWithMetadata`, `IssueKeyWithScopes`, cache holds scope
  set, `IssuedKey.Scopes`.
- `pkg/auth/auth.go` (modified): `AuthMiddleware` populates the ctx
  with the scope set; `MemoryKeyStore` becomes scope-aware via
  `AddWithScopes`.
- Per-service: `services/{ingest-gateway,query-engine,alert-manager}`
  add `auth.GinRequireScope(...)` to every authenticated route.
- `services/tenant-api/cmd/main.go`:
  `POST /v1/tenants/:id/api-keys` accepts a `scopes` array;
  `GET /v1/tenants/:id/api-keys` returns it.
- `tests/integration/tenant_api_test.go`: end-to-end scope
  round-trip against real Postgres.

No third-party packages added. No migration needed — the `scopes`
column already existed in Phase B-1's initial schema.

## Migration

Existing keys default to `[ingest]` (the SQL default). Operators that
need richer scopes must rotate. We deliberately did NOT auto-upgrade
existing keys to `AllScopes()`: that would convert the rollout into a
silent privilege escalation. The README documents the rotation
recipe.

## Alternatives considered

- **JWT + claims.** Cleaner once you have an OIDC issuer, but adds
  an issuer dependency and a public key distribution problem. Phase
  C-3b will add OIDC for operator login; per-tenant API keys stay on
  the opaque-token model because rotation/revocation is harder with
  JWTs (you need a revocation list anyway).

- **OPA / Rego policy.** Overkill for five scopes. Worth revisiting
  if we add attribute-based authorisation in C-4+.

- **Per-endpoint allowlist on the key.** Too granular; couples the
  key to the URL space. Scope vocabulary is the right level of
  abstraction.

## Verification

- `go test -race ./pkg/auth/...` exercises happy + sad paths,
  RequireScope allow/deny, ctx fallback, stateless-validator
  grants-all behavior.
- Integration test in `tests/integration/tenant_api_test.go`
  round-trips scopes through real Postgres.
- Every authenticated route now has a `GinRequireScope` guard
  (grep `GinRequireScope` in `services/`).
