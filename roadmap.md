# ObserveX: Distributed Observability & APM Platform — Production Roadmap

**Project:** ObserveX v1.0  
**Status:** Phase A + Phase B + Phase C (slices 1–3a) complete — Phase C-3b (OIDC, S3 cold tier) + Phase C-4 (UI) next  
**Duration:** 18 Weeks (14–18 Weeks)  
**Go Version:** 1.25+  
**Difficulty:** Mastery-Level

---

## 📋 Executive Summary

ObserveX is a **self-hosted, multi-tenant observability stack** that replaces commercial solutions like Datadog, New Relic, or Dynatrace. It ingests **metrics, logs, traces, and profiling data** from thousands of services simultaneously, stores them efficiently in a custom columnar engine, and surfaces insights through a real-time query interface and intelligent alerting.

## 🏗 Architectural Principles

- **Pipeline Architecture:** Composable stages via bounded channels for ingest.
- **Actor Model:** Isolated `TenantActor` goroutines for multi-tenant safety.
- **Lambda Architecture:** Speed layer (Redis) + Batch layer (DuckDB/S3) + Serving layer (ClickHouse).
- **Zero-Copy Data:** Apache Arrow IPC for inter-service data transfer.

---

## 📅 Phased Implementation Plan

### Phase 1: Ingest Foundation (delivered as Phase A) 🏗️ ✅

- [x] **Project Initialization:** Go workspace, ADR-0001 (Base Architecture).
- [x] **ingest-gateway:** HTTP/gRPC/StatsD receivers, mTLS & API-key validation.
- [x] **Pipeline Core:** `StageFunc` worker pool with back-pressure and load shedding.
- [x] **storage-engine (Core):** mmap WAL with CRC32 + recovery + group commit; ClickHouse native v2 driver behind a circuit breaker; embedded DDL migrations.
- [x] **Validation:** 1.5–2.2M signals/sec in benchmarks, far above the 12K/sec NFR.

### Phase 2 + 5 prereq: Multi-tenancy + observability (Phase B-1, B-4, B-5) 🎭 ✅

- [x] **tenant-api:** Postgres schema with **Row-Level Security**, embedded migrator, REST CRUD for tenants and API keys, append-only audit log, Argon2id key hashing.
- [x] **stream-processor:** OTP-flavoured supervisor with exponential-backoff restart + quarantine; per-service sliding-window CEP with edge-triggered firing; EWMA-baseline adaptive sampler with optional Redis state persistence.
- [x] **WASM Plugin System:** **wazero**-based host (pure Go, no CGo); JSON ABI; resource caps (memory + per-call deadline).

### Phase B-2: OTLP wire format ✅

- [x] Real **OTLP/HTTP** protobuf decoders for traces, metrics, logs at the standard `/v1/{traces,metrics,logs}` paths.
- [x] Gzip transparent; 8 MiB body cap; spec-compliant response codes; resource attributes flattened into Signal attrs.

### Phase 3: Query Engine & ObserveQL (Phase B-3) 🔍 ✅

- [x] **ObserveQL Grammar:** Go-native PEG via **participle** (ANTLR4 deferred; see ADR-0007 for rationale).
- [x] **query-engine:** HTTP service, allow-list-validated planner with mandatory tenant_id injection, ClickHouse executor, NDJSON streaming with header + trailer.
- [ ] **Phase C deferrals:** Arrow IPC codec, cost-based optimiser for joins/CTEs, federated S3 + DuckDB execution.

### Phase 4: Intelligence & Alerting (Phase B-5 + Phase C-1) 🧠 ✅

- [x] **ml-anomaly-detector skeleton:** rolling z-score (EWMA mean + variance) per (tenant, metric); HTTP ingest at `/v1/observations`; Prometheus anomaly counter.
- [x] **alert-manager (Phase C-1):** SLO burn-rate engine (multi-window multi-burn-rate per the Google SRE Workbook), Postgres-backed alert state with dedup + silence support, Slack / PagerDuty / Webhook notifier abstractions, CEP → alert-manager wire via `pkg/alertsink`. See ADR-0009.
- [ ] **Real ML (Phase C-3+):** ONNX Runtime integration for Isolation Forest / LSTM.

### Phase 5: UI & Production Hardening (Phase C) 🚀 partial

- [x] **Self-Observability (Phase C-2):** `pkg/selfobs` OTel SDK wrapper; every service emits traces back through the ingest-gateway OTLP loopback; default ParentBased(0.10) sampling. See ADR-0010.
- [x] **Deploy story (Phase C-2):** single multi-stage `build/docker/Dockerfile`; full `deploy/compose/docker-compose.yml` (Prometheus + Grafana + every service); minimal-but-real Helm chart at `deploy/helm/observex/` with ServiceMonitors. `helm lint` clean.
- [x] **API key scopes (Phase C-3a):** five canonical scopes (`ingest`, `query`, `alert.read`, `alert.write`, `tenant.admin`) enforced at every authenticated route via `auth.GinRequireScope`. tenant-api issuance accepts an explicit scope list. See ADR-0011.
- [x] **gRPC OTLP receiver (Phase C-3a):** canonical `TraceService` / `MetricsService` / `LogsService` mounted on `:4317` alongside the legacy `IngestService`. Auth interceptor enforces the `ingest` scope. See ADR-0012.
- [x] **Audit-log export (Phase C-3a):** `pkg/auditlog` with `FileExporter` (local NDJSON) and `S3Exporter` (object-lock COMPLIANCE WORM). tenant-api + alert-manager wire it via `buildAuditExporter`. See ADR-0013.
- [x] **GitOps (Phase C-3a):** `deploy/argocd/{appproject,application}.yaml` examples ride on top of the Helm chart.
- [ ] **Operator OIDC (Phase C-3b):** OIDC bearer-token validation in front of tenant-api admin endpoints (replaces the bootstrap admin token).
- [ ] **Cold Storage (Phase C-3b):** S3 + Parquet lifecycle for traces > 7d, metrics > 30d.
- [ ] **Real ML (Phase C-3b):** ONNX Runtime integration for Isolation Forest / LSTM.
- [ ] **ui-server (Phase C-4):** React dashboard served via `embed.FS`.

---

## 🛠️ Tooling & Stack

- **Languages:** Go 1.25, SQL, participle PEG (ObserveQL).
- **Data Stores:** ClickHouse (hot), PostgreSQL (control plane + alert state), Redis (optional sampler state), S3-compatible object store (audit-log WORM today via `pkg/auditlog`; Parquet cold tier in Phase C-3b).
- **Communication:** HTTP/JSON + NDJSON streams; OTLP over HTTP and gRPC (`:4318` and `:4317`); NATS JetStream (Phase C-3b).
- **Auth & Authz:** Argon2id-hashed bearer tokens with explicit per-key scopes (`ingest`, `query`, `alert.read`, `alert.write`, `tenant.admin`); operator OIDC for the control plane is Phase C-3b.
- **Observability of itself:** OTLP/HTTP loopback via `pkg/selfobs` (W3C TraceContext + ParentBased sampling), `/metrics` Prometheus endpoints on every service, pprof gated, Grafana dashboard at `deploy/grafana/dashboards/observex-overview.json`.
- **Plugins:** wazero (pure-Go WASM runtime).
- **Alerting:** Slack / PagerDuty / Webhook notifiers behind the `pkg/notifier` interface; SLO burn-rate per Google SRE Workbook.
- **Audit:** `pkg/auditlog` with file (NDJSON) and S3 backends; S3 Object-Lock COMPLIANCE mode for WORM retention.
- **DevOps:** `build/docker/Dockerfile` (distroless/static), Docker Compose for local, Helm chart at `deploy/helm/observex/` (lint clean), ArgoCD `AppProject` + `Application` examples at `deploy/argocd/`, GitHub Actions CI with helm-lint + kubeval + ArgoCD schema check.

---

## 📈 Non-Functional Requirements (NFRs)

- **Throughput:** 1B+ signals/day.
- **Ingest Latency:** <5ms P99 (to WAL).
- **Query Latency:** <500ms P99 (30-day range).
- **Compression:** 10:1 minimum.
- **Availability:** 99.9%.
