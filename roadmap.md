# ObserveX: Distributed Observability & APM Platform — Production Roadmap

**Project:** ObserveX v1.0  
**Status:** Phase A + Phase B complete — Phase C (production hardening) next  
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

### Phase 4: Intelligence & Alerting (Phase B-5 + Phase C) 🧠 partial

- [x] **ml-anomaly-detector skeleton:** rolling z-score (EWMA mean + variance) per (tenant, metric); HTTP ingest at `/v1/observations`; Prometheus anomaly counter.
- [ ] **alert-manager (Phase C):** SLO burn-rate engine, PagerDuty/Slack routing.
- [ ] **Real ML (Phase C):** ONNX Runtime integration for Isolation Forest / LSTM.

### Phase 5: UI & Production Hardening (Phase C) 🚀 pending

- [ ] **ui-server:** React dashboard served via `embed.FS`.
- [ ] **Cold Storage:** S3 + Parquet lifecycle for traces > 7d, metrics > 30d.
- [ ] **K8s & GitOps:** Helm charts + ArgoCD apps for all six services.
- [ ] **Self-Observability:** ObserveX scraping its own `/metrics` endpoints; OTLP exporter loopback for traces.
- [ ] **Audit log export** to S3 with object-lock for SOC2.
- [ ] **Operator OIDC** in front of tenant-api admin endpoints.
- [ ] **Read/write scopes** on API keys.

---

## 🛠️ Tooling & Stack

- **Languages:** Go 1.25, SQL, participle PEG (ObserveQL).
- **Data Stores:** ClickHouse (hot), PostgreSQL (control plane), Redis (optional sampler state), S3 + Parquet (cold tier — Phase C).
- **Communication:** gRPC (OTLP transport, Phase C), HTTP/JSON + NDJSON streams, NATS JetStream (Phase C).
- **Observability:** OTLP/HTTP receivers, `/metrics` Prometheus endpoints on every service, pprof gated.
- **Plugins:** wazero (pure-Go WASM runtime).
- **DevOps:** Docker Compose (local), Helm + ArgoCD (Phase C), GitHub Actions (CI today).

---

## 📈 Non-Functional Requirements (NFRs)

- **Throughput:** 1B+ signals/day.
- **Ingest Latency:** <5ms P99 (to WAL).
- **Query Latency:** <500ms P99 (30-day range).
- **Compression:** 10:1 minimum.
- **Availability:** 99.9%.
