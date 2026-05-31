# ADR-0013 — Audit-log export (tamper-evident, S3 Object-Lock)

- Status: Accepted
- Date: 2026-05-31
- Phase: C-3a

## Context

ObserveX already captures audit-worthy events in Postgres:

- `tenant_audit_log` in tenant-api covers tenant CRUD, API key
  issuance/revocation, and other control-plane mutations.
- `alert_history` in alert-manager covers every alert state
  transition.

Captured audit records are durable as long as the application
database is, but SOC2 (and similar regulated frameworks) expects
audit records to **outlive any single compromise of the application
stack**. A database administrator with write access to Postgres can
silently rewrite history; an attacker who reaches the postgres host
can do the same. The mitigation is to fan-out audit records to a
storage layer where:

1. records are append-only, AND
2. records cannot be deleted within a retention window
   (write-once-read-many, "WORM").

S3 Object Lock (specifically the `COMPLIANCE` mode) provides exactly
this contract — once written, the object cannot be deleted or
overwritten by anyone, including the root account, until the
retention period elapses. AWS, MinIO ≥ RELEASE.2024, Cloudflare R2,
and Backblaze B2 all implement the same API.

## Decision

Introduce `pkg/auditlog` as the single export seam:

```go
type Exporter interface {
    Append(ctx context.Context, r Record) error
    Close(ctx context.Context) error
}
```

Concrete implementations:

- `NopExporter` — default when no backend is configured.
- `FileExporter` — newline-delimited JSON to a local file with
  `fsync` per record. Used in tests, local dev, and air-gapped
  deploys.
- `S3Exporter` — batches records on a configurable
  interval/count and uploads each batch as one NDJSON object under
  `audit/YYYY/MM/DD/HH/{epoch_ms}-{rand}.ndjson`. When
  `OBSERVE_X_AUDIT_LOG_S3_LOCK` is set to `GOVERNANCE` or
  `COMPLIANCE`, the upload carries the corresponding
  `x-amz-object-lock-mode` and `x-amz-object-lock-retain-until-date`
  headers via the AWS SDK v2.

`BufferedExporter` wraps any inner exporter to keep the synchronous
request path off the cloud upload latency tail. Records that don't
fit in the buffer are NOT dropped — they fall back to a synchronous
Append so callers observe back-pressure rather than data loss.
Records that error on upload are pushed back into the pending batch
and retried on the next flush; this accepts at-most-once duplication
under network failure in exchange for at-least-once durability.

Tenant-api and alert-manager wire the exporter via the same
`buildAuditExporter(ctx, logger)` env-driven helper:

```
OBSERVE_X_AUDIT_LOG_BACKEND     = file | s3
OBSERVE_X_AUDIT_LOG_FILE_PATH   = path
OBSERVE_X_AUDIT_LOG_S3_BUCKET   = bucket
OBSERVE_X_AUDIT_LOG_S3_PREFIX   = audit/
OBSERVE_X_AUDIT_LOG_S3_REGION   = us-east-1
OBSERVE_X_AUDIT_LOG_S3_ENDPOINT = s3.example.com (optional, MinIO etc.)
OBSERVE_X_AUDIT_LOG_S3_LOCK     = GOVERNANCE | COMPLIANCE | ""
OBSERVE_X_AUDIT_LOG_S3_RETAIN   = 8760h        (1 year default)
```

## Trade-offs

- **Two writes (Postgres + exporter).** Tenant-api writes both the
  Postgres row and the exporter record. The Postgres write is the
  source of truth for application reads; the exporter write is the
  immutable trail. If only the Postgres write succeeds, the exporter
  log misses one record; if only the exporter write succeeds, the
  app shows nothing but the record is preserved. We accept this small
  inconsistency window over the alternative (transactional outbox)
  because the outbox adds operational complexity disproportionate to
  the value.

- **Batch flushes vs per-record uploads.** Per-record S3 PUTs are
  slow and expensive. We batch on time + count. Worst-case loss
  window if the service crashes mid-flush: one flush interval (60s
  default). For compliance-grade workloads with strict RPO, drop
  `FlushInterval` to 5s and pay the PUT cost.

- **At-least-once on retry.** Network failures cause the batch to be
  re-pushed onto the pending buffer. On the next flush a NEW object
  key is generated (different epoch_ms suffix), so the same batch
  may be uploaded twice. Downstream consumers MUST de-dupe by
  `Record.ID` if they care; the immutable trail correctly contains
  the union of records.

- **No client-side encryption.** S3 SSE-KMS is the right answer
  here, configured at the bucket level. We could pass
  `ServerSideEncryption: aws.SSE_KMS` through the PutObjectInput,
  but most operators configure default bucket encryption and don't
  want service-level overrides. Left as a follow-up for a workload
  with explicit per-object key requirements.

- **No PII redaction.** The audit records carry the same fields the
  Postgres tables carry. If your tenant identifiers or actor strings
  are PII, redact upstream of `Append`.

## Package changes

- `pkg/auditlog/auditlog.go` (new): `Record`, `Exporter`,
  `NopExporter`, `FileExporter`, `BufferedExporter`.
- `pkg/auditlog/s3.go` (new): `S3Exporter`, `S3Options`,
  object-lock plumbing.
- `pkg/auditlog/auditlog_test.go` (new): unit tests including
  back-pressure semantics for `BufferedExporter`.
- `services/tenant-api/cmd/main.go`: `buildAuditExporter` helper,
  pushes a record after every Postgres audit write.
- `services/alert-manager/cmd/main.go`: same helper, emits records
  for `silence.create` and `slo.register` (alert state transitions
  are still captured in `alert_history`; a follow-up will fan those
  through the exporter too).

New dependencies: `github.com/aws/aws-sdk-go-v2/config`,
`github.com/aws/aws-sdk-go-v2/service/s3`, +
`github.com/aws/aws-sdk-go-v2/service/s3/types`. The AWS SDK v2 is
modular; only S3 + config are pulled in, plus the standard transitive
graph (sso, sts, signers, etc.). Footprint addition: ~2.5 MB to the
linked binaries that use the s3 backend; the file-only build does not
pull the AWS code paths.

## Migration

Existing deployments without `OBSERVE_X_AUDIT_LOG_BACKEND` set get
`NopExporter` — zero behavior change. Operators opt in by setting
the env vars; the bucket itself must be pre-created with Object Lock
ENABLED at creation time (it cannot be enabled on an existing
bucket per AWS).

## Alternatives considered

- **Postgres logical replication to a regulated read replica.**
  Strong, but the replica is still a database with admins. Doesn't
  meet the SOC2 "outlive compromise of the app stack" bar without
  also locking down the replica's filesystem with WORM storage —
  which gets us right back to S3.

- **Kafka with infinite retention.** Adds an operational dependency
  (a Kafka cluster) and Kafka topics are mutable by anyone with
  cluster admin. Object Lock S3 is the simpler and stronger
  guarantee.

- **Event-sourced architecture inside Postgres.** Would mean
  rewriting the tenant-api and alert-manager domain models. Too big
  a change for the audit guarantee we need.

## Verification

- `go test -race ./pkg/auditlog/...`: validates `Record.Validate`,
  `FileExporter` round-trip, `NopExporter` no-error, and that
  `BufferedExporter` never drops records under back-pressure (a
  burst of 20 records against a slow inner with buffer size 4 still
  persists all 20).
- The S3 path is exercised in CI via MinIO in `docker-compose` (a
  follow-up PR; not part of this slice).
