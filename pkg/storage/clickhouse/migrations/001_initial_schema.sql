-- 001 — initial schema for ObserveX (Phase A).
--
-- These DDLs are idempotent (CREATE TABLE IF NOT EXISTS) and are executed
-- on connection by the ClickHouse backend. They intentionally use plain
-- MergeTree, not ReplicatedMergeTree, so the schema runs against both
-- single-node dev and clustered prod without conditional logic. The
-- production Helm chart will swap `engine` via a templated macro in
-- Phase C; the application code does not care.

CREATE TABLE IF NOT EXISTS metrics (
    tenant_id   String,
    metric_name LowCardinality(String),
    timestamp   DateTime64(3, 'UTC'),
    value       Float64,
    labels      Map(String, String),
    received_at DateTime64(3, 'UTC') DEFAULT now64()
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, metric_name, timestamp)
TTL toDateTime(timestamp) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS logs (
    tenant_id    String,
    service_name LowCardinality(String),
    severity     LowCardinality(String),
    body         String CODEC(ZSTD(3)),
    attributes   Map(String, String),
    timestamp    DateTime64(3, 'UTC'),
    trace_id     String,
    span_id      String
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, service_name, timestamp)
TTL toDateTime(timestamp) + INTERVAL 14 DAY
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS traces (
    tenant_id      String,
    trace_id       String,
    span_id        String,
    parent_span_id String,
    operation_name LowCardinality(String),
    service_name   LowCardinality(String),
    start_time     DateTime64(6, 'UTC'),
    end_time       DateTime64(6, 'UTC'),
    duration_ns    Int64,
    attributes     Map(String, String),
    status_code    LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(start_time)
ORDER BY (tenant_id, trace_id, start_time)
TTL toDateTime(start_time) + INTERVAL 7 DAY;
