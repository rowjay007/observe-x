-- 002 — cold-tier lifecycle (Phase C-3b, see ADR-0015).
--
-- This migration is a no-op if the operator hasn't deployed the
-- companion `storage_policies.xml` file that declares the `cold_s3`
-- disk and the `hot_cold` policy. Without the policy ClickHouse
-- treats every TTL ... TO DISK clause as a hard error, so we use a
-- conditional ALTER guarded by the policies the server exposes via
-- the `system.storage_policies` table.
--
-- We DON'T modify ORDER BY / PARTITION BY (which would force a full
-- rebuild). We only set:
--   * `storage_policy` so future parts pick up the multi-disk policy,
--   * a new TTL with MOVE TO DISK to push aged parts to S3,
--   * a delete-after window that's longer than the move window.
--
-- Per-table retention windows (configurable in production via
-- additional ALTERs from the operator's tenancy tool):
--
--   metrics  hot 30d → cold 90d → delete
--   logs     hot 14d → cold 30d → delete
--   traces   hot  7d → cold 30d → delete
--
-- These are the defaults; per-tenant retention overrides land via
-- `ALTER TABLE … MODIFY TTL … WHERE tenant_id = $tenant`.

-- ── Bail early if the hot_cold policy isn't installed. ──────────────────
-- ClickHouse SQL can't express conditional DDL natively, so we
-- emit the ALTER unconditionally and tolerate the error in the
-- migrator (see store_migrate.go). The expected error code is
-- ERR_NO_DISK (243) — the migrator skips ahead on that specific
-- code so a single-disk dev cluster keeps working.

ALTER TABLE metrics MODIFY SETTING storage_policy = 'hot_cold';
ALTER TABLE logs    MODIFY SETTING storage_policy = 'hot_cold';
ALTER TABLE traces  MODIFY SETTING storage_policy = 'hot_cold';

ALTER TABLE metrics MODIFY TTL
    toDateTime(timestamp) + INTERVAL 30 DAY  TO DISK 'cold_s3',
    toDateTime(timestamp) + INTERVAL 90 DAY  DELETE;

ALTER TABLE logs MODIFY TTL
    toDateTime(timestamp) + INTERVAL 14 DAY  TO DISK 'cold_s3',
    toDateTime(timestamp) + INTERVAL 30 DAY  DELETE;

ALTER TABLE traces MODIFY TTL
    toDateTime(start_time) + INTERVAL 7  DAY TO DISK 'cold_s3',
    toDateTime(start_time) + INTERVAL 30 DAY DELETE;
