# ADR-0021 — Operator audit aggregation in the UI

- Status: Accepted
- Date: 2026-06-01
- Phase: D-4

## Context

Audit records have always landed in `tenant_audit_log` and (via
the `pkg/auditlog` exporter) in S3 WORM storage. Querying them
required `psql` against the control-plane database or pulling
files from S3. Neither is suitable for the SRE-on-call use case
of "who issued this API key" or "did anyone change retention in
the last hour."

## Decision

Surface the audit log in the operator UI:

- New endpoint `GET /v1/audit?tenant_id=&limit=` on `tenant-api`.
- Returns the latest N records from `tenant_audit_log`, optional
  tenant filter, hard cap at 500 per call.
- Phase C-4 UI gains an "Audit" tab driven by the same endpoint.

## Trade-offs

- **Latest-N rather than full paging** — operators are looking
  for "what just happened," not retrospective forensics. For the
  latter, the S3 audit export (ADR-0013) remains the source of
  truth and is queryable from Athena / DuckDB.
- **No filter on actor or action** — the JSON body carries
  `actor` and `action`; the client can filter in-browser. Adding
  query params here is cheap when needed.
- **Read-only** — the UI cannot delete audit records (and the
  underlying table has no DELETE permission for the application
  user). A separate retention job trims rows beyond 90 days from
  the hot DB; full history lives in WORM.

## Package changes

- `services/tenant-api/store/store.go` — `ListAudit` method.
- `services/tenant-api/cmd/main.go` — `listAudit` handler bound
  to the admin route group.
- `services/ui-server/cmd/assets/{index.html,app.js,app.css}` —
  new tab, table, refresh action.

## Verification

- `go test ./services/tenant-api/store/...` exercises the query.
- UI smoke test in CI confirms the Audit tab renders.
