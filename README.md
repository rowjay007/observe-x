# observe-x

Distributed observability and APM platform written in Go. Self-hosted multi-tenant ingestion, processing, storage, and query engine for metrics, logs, traces, and profiling data.

## Quick Start

### Prerequisites

- Go 1.23+
- ClickHouse 23.x (for storage backend)
- Docker & Docker Compose (for local development)

### Environment Setup

The ingest gateway requires an API secret for key generation:

```bash
export OBSERVE_X_API_SECRET="your-secret-key-here"
```

### Running Locally

```bash
# Start ClickHouse and other dependencies
docker-compose -f tests/docker-compose.yml up -d

# Build ingest gateway
go build -o observe-x-ingest ./services/ingest-gateway/cmd

# Run the server
./observe-x-ingest
# Server listens on http://localhost:4318
```

### API Usage

#### Health Check

```bash
curl http://localhost:4318/health
```

#### Ingest Data

API keys follow the format: `{tenant_id}:{blake3_hash}`. For testing:

```bash
curl -X POST http://localhost:4318/v1/ingest \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer {API_KEY}" \
  -d '{
    "tenant_id": "tenant-123",
    "type": "metric",
    "payload": "eyJzZXJ2aWNlIjoiYXBpIn0=",
    "attributes": {"source": "prometheus"}
  }'
```

## Architecture

### Services

- **ingest-gateway** (`services/ingest-gateway/cmd`)
  - OTLP HTTP receiver on port 4318
  - API key validation with BLAKE3 hashing
  - Auth middleware enforces `Authorization: Bearer` header
  - Tenant isolation via X-Tenant-ID header

- **storage-engine** (`services/storage-engine`)
  - Write-ahead log (WAL) implementation
  - ClickHouse integration for persistent storage
  - Batch writes and query support

- **stream-processor** (`services/stream-processor`)
  - Actor model for multi-tenant isolation
  - Signal processing pipeline
  - Complex event processing (CEP) engine

## Development

### Running Tests

```bash
# All tests
go test ./...

# Ingest gateway auth tests
go test ./services/ingest-gateway/internal/auth -v

# Integration tests
go test ./tests/integration/...

# Benchmarks
go test -bench=. ./tests/benchmarks/...
```

### Build

```bash
# Ingest gateway
go build -o observe-x-ingest ./services/ingest-gateway/cmd

# Storage engine (library)
go build ./services/storage-engine/...

# Stream processor (library)
go build ./services/stream-processor/...
```

## Documentation

- [Roadmap](./roadmap.md) - Development roadmap and phases

## Status

**Phase 1: Ingest Foundation** (Active)
- ✅ Project initialization and workspace setup
- ✅ ingest-gateway with OTLP HTTP receiver
- ✅ API key authentication (BLAKE3 hashing)
- ✅ Custom WAL with ClickHouse backend
- ✅ Multi-tenant actor model
- 🔄 Performance validation (12K events/sec target)

## License

Proprietary - All rights reserved
