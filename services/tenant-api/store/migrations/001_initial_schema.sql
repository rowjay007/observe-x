-- 001 — initial tenant control-plane schema.
--
-- Three tables (tenants, tenant_api_keys, tenant_audit_log) plus the
-- supporting RLS infrastructure. Idempotent on re-run because the
-- migrator already gates on schema_migrations.
--
-- See docs/adr/0004-tenant-control-plane.md for the rationale.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ── tenants ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tenants (
    id              TEXT PRIMARY KEY,
    display_name    TEXT        NOT NULL,
    tier            TEXT        NOT NULL DEFAULT 'free'
                    CHECK (tier IN ('free', 'pro', 'enterprise')),
    retention_days  INT         NOT NULL DEFAULT 14
                    CHECK (retention_days BETWEEN 1 AND 365),
    quota_eps       INT         NOT NULL DEFAULT 1000
                    CHECK (quota_eps >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

-- ── api keys ─────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tenant_api_keys (
    kid             TEXT PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    hash            TEXT        NOT NULL,         -- Argon2id encoded
    prefix          TEXT        NOT NULL,         -- non-secret human ID
    scopes          TEXT[]      NOT NULL DEFAULT ARRAY['ingest'],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS tenant_api_keys_active_idx
    ON tenant_api_keys (tenant_id)
    WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS tenant_api_keys_prefix_idx
    ON tenant_api_keys (prefix);

-- ── audit log ────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tenant_audit_log (
    id          BIGSERIAL   PRIMARY KEY,
    tenant_id   TEXT,                              -- NULL for system-level events
    actor       TEXT        NOT NULL,              -- 'admin', 'system', or tenant id
    action      TEXT        NOT NULL,
    details     JSONB,
    source_ip   INET,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tenant_audit_log_tenant_time_idx
    ON tenant_audit_log (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS tenant_audit_log_action_idx
    ON tenant_audit_log (action);

-- ── Row-Level Security ───────────────────────────────────────────────────
--
-- RLS is a control-plane safety net: even if application code forgets a
-- WHERE tenant_id = $X clause, Postgres rejects the cross-tenant read.
-- See ADR-0004 for why we enable it on every tenant-owned table.

ALTER TABLE tenants            ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_api_keys    ENABLE ROW LEVEL SECURITY;
-- audit log stays globally readable by admin; tenants don't read each
-- other's audit events through this table.

-- Policies key on a session-local setting (`app.tenant_id`) that the
-- application sets via `SET LOCAL app.tenant_id = '<id>'` at the start
-- of every tenant-scoped request. Admin connections do not set the
-- setting; they bypass RLS via BYPASSRLS on the role.

DROP POLICY IF EXISTS tenants_isolation ON tenants;
CREATE POLICY tenants_isolation ON tenants
    USING (id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS api_keys_isolation ON tenant_api_keys;
CREATE POLICY api_keys_isolation ON tenant_api_keys
    USING (tenant_id = current_setting('app.tenant_id', true));
