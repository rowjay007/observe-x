# ObserveX Roadmap: Distributed Observability & APM Platform

ObserveX is a production-grade, self-hosted observability stack built in Go 1.23+. This roadmap outlines the iterative delivery of 8 microservices, a custom query language (ObserveQL), and an AI-driven anomaly detection engine.

## 🏗 Architectural Principles
- **Pipeline Architecture:** Composable stages via bounded channels for ingest.
- **Actor Model:** Isolated `TenantActor` goroutines for multi-tenant safety.
- **Lambda Architecture:** Speed layer (Redis) + Batch layer (DuckDB/S3) + Serving layer (ClickHouse).
- **Zero-Copy Data:** Apache Arrow IPC for inter-service data transfer.

---

## 📅 Phased Implementation Plan

### Phase 1: Ingest Foundation (Weeks 1-3) 🏗️
- [ ] **Project Initialization:** Go workspace, Proto definitions, and ADR-001 (Base Architecture).
- [ ] **ingest-gateway:**
    - OTLP gRPC/HTTP receivers.
    - StatsD UDP listener.
    - mTLS & API Key (BLAKE3) validation.
- [ ] **Pipeline Core:** `StageFunc` implementation with back-pressure and load shedding.
- [ ] **storage-engine (Core):**
    - Custom WAL (mmap'd segments, CRC32).
    - ClickHouse Strategy implementation.
    - DDL Migrations.
- [ ] **Validation:** Prove 12K events/sec ingest with <5ms P99 WAL commit.

### Phase 2: Stream Processing & Multi-Tenancy (Weeks 4-7) 🎭
- [ ] **stream-processor:**
    - Actor Model implementation (TenantActor + Supervisor).
    - CEP (Complex Event Processing) engine.
    - Adaptive Sampler (Priority-Queue based).
- [ ] **tenant-api:**
    - PostgreSQL schema with RLS (Row-Level Security).
    - GraphQL management API.
- [ ] **WASM Plugin System:** `wasmtime-go` integration for tenant-specific logic.

### Phase 3: Query Engine & ObserveQL (Weeks 8-11) 🔍
- [ ] **ObserveQL Grammar:** ANTLR4 definition and Go parser generation.
- [ ] **query-engine:**
    - Distributed Query Planner.
    - Cost-Based Optimizer.
    - Federated Execution (ClickHouse + S3).
    - gRPC Result Streaming (Arrow format).

### Phase 4: Intelligence & Alerting (Weeks 12-14) 🧠
- [ ] **ml-anomaly-detector:**
    - ONNX Runtime integration (Isolation Forest + LSTM).
    - Real-time EWMA scoring.
- [ ] **alert-manager:**
    - SLO burn-rate evaluation engine.
    - Notification routing tree (PagerDuty, Slack).

### Phase 5: UI & Production Hardening (Weeks 15-18) 🚀
- [ ] **ui-server:** React + D3.js dashboard served via `embed.FS`.
- [ ] **Cold Storage:** S3 Parquet + Delta Lake lifecycle management.
- [ ] **K8s & GitOps:** Helm charts + ArgoCD configurations.
- [ ] **Self-Observability:** ObserveX monitoring itself.

---

## 🛠️ Tooling & Stack
- **Languages:** Go 1.23, SQL, ANTLR4.
- **Data Stores:** ClickHouse, PostgreSQL, Redis, S3 (Parquet).
- **Communication:** gRPC, Apache Arrow, NATS JetStream.
- **Observability:** OTLP, pprof, OpenTelemetry.
- **DevOps:** Terraform, Kubernetes, Helm, k6.

---

## 📈 Non-Functional Requirements (NFRs)
- **Throughput:** 1B+ signals/day.
- **Ingest Latency:** <5ms P99 (to WAL).
- **Query Latency:** <500ms P99 (30-day range).
- **Compression:** 10:1 minimum.
- **Availability:** 99.9%.
