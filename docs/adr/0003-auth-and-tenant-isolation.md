# ADR 0003 — Auth and tenant isolation

- **Status:** Accepted (Phase A) — known limitations carried into Phase B
- **Date:** 2026-05-29

## Context

The pre-Phase-A authentication model used a single environment-level
secret (`OBSERVE_X_API_SECRET`) to derive every tenant's API key as
`tenant_id : blake3(secret || ":" || tenant_id)`. This has three
critical problems for production:

1. **Single point of compromise.** Leak of `OBSERVE_X_API_SECRET`
   lets an attacker mint a valid key for every tenant in existence.
2. **No rotation, no revocation.** The derived key is deterministic;
   you cannot invalidate a leaked key without invalidating every key
   for that tenant.
3. **No key identity.** Audit logs cannot distinguish "key A used by
   service X" from "key B used by service Y" within the same tenant.

We cannot fully fix this in Phase A because the tenant control plane
(Postgres + RLS + GraphQL API) is the Phase B deliverable. What we
can do is define a `KeyStore` interface today that survives the
switch.

## Decision

### Phase A — interface, two implementations

```go
type KeyStore interface {
    ValidateKey(key string) (tenantID string, valid bool)
}
```

Implementations shipped in Phase A:

- `StatelessKeyValidator` — the existing single-secret derivation,
  preserved for dev/test only. The README now documents that this
  mode is "dev only".
- `MemoryKeyStore` — keys are stored as BLAKE3 hashes, support
  per-key revocation, and reject tenant-id spoofing in constant
  time. Used by tests and by Phase B as scaffolding before Postgres
  lands.

### Phase B — Postgres-backed store (planned, not in this PR)

`PostgresKeyStore` will:

- Store per-tenant keys as `(tenant_id, kid, argon2id_hash, scopes,
  revoked_at, expires_at, rotation_generation)`.
- Use Argon2id at **issuance** (cost-tolerant) and BLAKE3 at the
  **per-request lookup** path (cost-sensitive). Issuance is rare and
  can afford 50–100 ms; per-request validation must stay under
  100 µs to support the 12 K events/sec NFR.
- Apply Postgres Row-Level Security so application bugs cannot read
  another tenant's keys.
- Cache validations in memory with a short TTL (5 s) and bound the
  cache to the active tenant set.

Short-lived bearer tokens (JWT signed with rotating Ed25519, `kid`
in header) will fan out from the tenant-api so high-RPS paths never
touch Postgres at all.

### mTLS as alternative auth

When `OBSERVE_X_REQUIRE_MTLS=true` is set in Phase B, tenant identity
is derived from the client certificate's SAN field and the API-key
header is ignored. The CA is a per-deployment trust anchor managed by
the operator (or, in the SaaS model, by us).

## Consequences

### Positive

- The `KeyStore` interface is the only seam consumers depend on; we
  can replace the implementation without touching the receivers.
- `MemoryKeyStore` already gives tests realistic semantics (add,
  revoke, reject spoofed tenant id) without external services.
- The "single secret signs everyone" anti-pattern is documented as a
  dev-only escape hatch with an explicit README warning.

### Negative

- Production deployments running today's `StatelessKeyValidator` are
  insecure-by-design and need to migrate before any external tenant
  onboards. We document this loudly.
- Argon2id introduces CPU cost at key issuance — fine because keys
  are issued rarely, not validated rarely.

## Open questions for Phase B

- Should JWT short-lived tokens be the *only* runtime credential, or
  should long-lived API keys remain a valid path? (Lean: both, with
  the API key restricted to `kid`-prefixed JWT exchange.)
- How does mTLS auth interact with the per-tenant quota counters?
  (Lean: cert SAN → tenant id → same quota lookup as API key.)
