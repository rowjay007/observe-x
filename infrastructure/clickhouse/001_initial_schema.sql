-- Create metrics table with ReplicatedMergeTree for high availability and performance
CREATE TABLE IF NOT EXISTS metrics (
    tenant_id String,
    metric_name String,
    timestamp DateTime64(3, 'UTC'),
    value Float64,
    labels Map(String, String),
    updated_at DateTime64(3, 'UTC') DEFAULT now64()
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, metric_name, timestamp)
SETTINGS index_granularity = 8192;

-- Create logs table with Zstd compression for high storage efficiency
CREATE TABLE IF NOT EXISTS logs (
    tenant_id String,
    service_name String,
    severity String,
    body String,
    attributes Map(String, String),
    timestamp DateTime64(3, 'UTC'),
    trace_id String,
    span_id String
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant_id, service_name, timestamp)
SETTINGS index_granularity = 8192;

-- Create traces table for span storage
CREATE TABLE IF NOT EXISTS traces (
    tenant_id String,
    trace_id String,
    span_id String,
    parent_span_id String,
    operation_name String,
    service_name String,
    start_time DateTime64(6, 'UTC'),
    end_time DateTime64(6, 'UTC'),
    duration_ns Int64,
    attributes Map(String, String),
    status_code String
) ENGINE = MergeTree()
PARTITION BY toYYYYMMDD(start_time)
ORDER BY (tenant_id, trace_id, start_time);
