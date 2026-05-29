# ADR 0004 — Tenant control plane

- **Status:** Accepted (Phase B-1)
- **Date:** 2026-05-29
- **Supersedes parts of:** ADR-0003 (auth and tenant isolation) — see
  "Open questions" section there

## Context

Phase A documented the dev-only `StatelessKeyValidator` as an
explicit security debt: one shared secret signs every tenant, leaks
compromise every tenant, no rotation, no revocation, no audit trail.
Phase B-1 retires that debt for production deployments while keeping
the dev-only path available for local work.

ObserveX is multi-tenant by design. The tenant control plane is the
only place where new tenants are created, API keys are issued and
revoked, retention and quota are configured, and audit trails are
written. Every data-plane service (ingest-gateway, future
stream-processor, future query-engine) is a read-only consumer of
this control plane's data.

## Decision

### A separate service (`services/tenant-api`)

The control plane is its own deployable. Reasons:

1. **Different blast radius.** A bug in ingest does not get to mutate
   tenant records or issue keys.
2. **Different scaling profile.** Control plane sees thousands of
   QPS at most; data plane sees millions. Co-locating couples them
   to the wrong cost curve.
3. **Different auth model.** Control plane is admin-token (Phase B-1) /
   operator-OIDC (Phase B-3+). Data plane is per-tenant API keys.

### Postgres for state, RLS for safety

We use Postgres because the access pattern is small-cardinality CRUD
with transactional consistency requirements — exactly what relational
stores were built for. ClickHouse is the wrong tool here (no row
updates, weak ACID).

Row-Level Security is enabled on every tenant-owned table
(`tenants`, `tenant_api_keys`). Even if an application bug forgets a
`WHERE tenant_id = $X` clause, Postgres refuses the cross-tenant
read. Tenant-scoped connections set `app.tenant_id` via `SET LOCAL`
at the start of each request; admin connections run as a `BYPASSRLS`
role and see everything.

### Wire key format: `tenant_id:kid:raw_secret`

```
acme-corp:a3f2c901bb04:Ovr2…Q  ← raw key, shown once at issuance
└── tenant ┘└─ 12-char ─┘└── 43-char base64url secret ──┘
              kid              (32 bytes of crypto/rand)
```

Three parts because:

- **tenant_id** is the SQL filter — non-secret.
- **kid** identifies exactly one row in `tenant_api_keys` — non-secret.
  Without it we'd have to argon2-verify against every active key for
  the tenant, which is `O(N×50ms)`.
- **secret** is the high-entropy material that's actually checked.

Two-part keys (the dev-only `StatelessKeyValidator` format) are
explicitly rejected by `PostgresKeyStore.ValidateKey`, so a
misconfigured deployment cannot accidentally accept dev keys.

### Argon2id at issuance, BLAKE3-keyed cache at validation

```
issuance (rare, ~50ms):     hashArgon2id(secret) → stored in DB
                            time=3, memory=64MiB, threads=2, keyLen=32  (OWASP)

validation (hot, must be ≪1ms):
  cache hit  → constant-time compare on cached tenantID
  cache miss → SELECT by (tenant_id, kid) → argon2id.Verify
               → put(blake3(wire_key), tenantID) for 5s
```

The cache is bounded (default 10 000 entries), TTL'd (5s), and uses
BLAKE3 of the *full wire key* as the key — so a leaked cache snapshot
does not give an attacker the raw secret. Tenant-id spoofing is
caught with a constant-time compare on the cached value.

`last_used_at` is updated asynchronously, debounced per kid (default
1 minute), via a buffered channel; updates that miss the buffer are
dropped to ensure the validation hot path never blocks on Postgres
write latency.

### Audit log is append-only

Every mutation in tenant-api writes to `tenant_audit_log` with:
actor, action, target tenant, details JSON, source IP, timestamp.
The table is indexed on `(tenant_id, created_at desc)` for per-tenant
audit reads and on `action` for compliance reports. Nightly export
to S3 with object-lock (WORM) is a Phase C deliverable.

### Migration strategy

A tiny in-package migrator (`services/tenant-api/internal/store/
migrate.go`) reads embedded SQL files in lex order, runs each in a
transaction, and records applied versions in `schema_migrations`.
Append-only: never rewrite existing files. We avoided pulling in
goose because the migrator is small enough to audit in one sitting
and a shared dep would have to be justified at the org level.

## Consequences

### Positive

- The Phase A security debt is paid down for production. Leaked keys
  can be revoked individually; the blast radius of any single leak
  is one tenant, not all of them.
- The data plane's auth surface is unchanged — `PostgresKeyStore`
  implements the same `KeyStore` interface as the dev validator, so
  the ingest-gateway switch is a constructor swap.
- RLS gives us a defense-in-depth guarantee that survives code-review
  misses; cross-tenant data leaks now require both an app bug *and* a
  misconfigured database role.
- Cache + async writes keep the validation hot path well under 100µs
  in the steady state, so the 12K/sec NFR continues to hold.

### Negative

- One more service to deploy (binary + Postgres). The dev experience
  costs `docker compose up postgres` and a one-line env var.
- Argon2id at issuance is CPU-heavy (~50ms each). Acceptable because
  issuance is rare and not on any user-facing path.
- The validation cache's eviction is "drop 10% on capacity overflow"
  rather than true LRU. Good enough at 5s TTL; revisit if production
  metrics show cache thrashing.

### Go toolchain bump

`github.com/jackc/pgx/v5@v5.9.2` requires Go 1.25+. We accept the
forward bump (previously 1.24 from Phase A) — additive only, no
breaking changes consumed.

## Alternatives considered

- **JWT short-lived tokens only.** Considered for the data plane.
  Deferred to Phase B-3 when the auth refresh path (key-issued JWTs
  signed by a rotating Ed25519 key) lands together with the operator
  control plane. Phase B-1 sticks with long-lived API keys because
  that is what the existing OTel SDKs expect.
- **mTLS as the only auth.** Operationally heavy for SaaS deployments
  where customers don't want to manage certs. mTLS support stays in
  `pkg/auth.TLSConfig` and the gateway honours `OBSERVE_X_TLS_CA_FILE`
  for client-cert verification, but it is opt-in, not the default.
- **gRPC-only tenant-api with gqlgen later.** Lower friction during
  Phase B-1; REST is the lingua franca for `curl`-based bootstrap
  and easy to evolve to GraphQL in Phase B-3 if the UI demands it.
