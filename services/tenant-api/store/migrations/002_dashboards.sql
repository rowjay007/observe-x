-- 002 — dashboards table (Phase E-4).
--
-- Stores the panel layout an operator builds in the Metrics tab. The
-- layout is opaque JSONB on the server side (the SPA owns the schema)
-- so we don't have to redeploy tenant-api when the UI evolves new
-- panel types. Server-side validation is limited to "valid JSON" and
-- a 1 MiB body cap upstream.
--
-- Tenant scope: every row carries a tenant_id and RLS pins reads to
-- the session's app.tenant_id setting, mirroring the existing
-- isolation policies for `tenants` and `tenant_api_keys`.
--
-- Sharing: dashboards are tenant-scoped; cross-tenant sharing is NOT
-- supported in v1 by design (multi-tenant safety > convenience). An
-- operator with admin scope on tenant A can clone-and-paste the
-- exported JSON into tenant B via the UI Import button.

CREATE TABLE IF NOT EXISTS dashboards (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    TEXT        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    layout       JSONB       NOT NULL,                      -- {panels: [...]}
    created_by   TEXT,                                       -- audit trail
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS dashboards_tenant_idx
    ON dashboards (tenant_id, updated_at DESC);

-- Name unique per tenant so the "Open" button is unambiguous. The
-- UI always assigns a name; if the operator pastes a duplicate the
-- INSERT fails and the SPA falls back to PUT-by-id.
CREATE UNIQUE INDEX IF NOT EXISTS dashboards_tenant_name_uq
    ON dashboards (tenant_id, name);

ALTER TABLE dashboards ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS dashboards_isolation ON dashboards;
CREATE POLICY dashboards_isolation ON dashboards
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Trigger to keep updated_at honest. Saves the application layer
-- from forgetting on PATCH paths.
CREATE OR REPLACE FUNCTION touch_dashboards_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS dashboards_touch_updated ON dashboards;
CREATE TRIGGER dashboards_touch_updated
    BEFORE UPDATE ON dashboards
    FOR EACH ROW EXECUTE FUNCTION touch_dashboards_updated_at();
