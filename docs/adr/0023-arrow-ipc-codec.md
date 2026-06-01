# ADR-0023 — Arrow IPC codec for query-engine results

- Status: Accepted
- Date: 2026-06-01
- Phase: D-6

## Context

Phase B-3 ships NDJSON as the query-engine wire format. It's the
right choice for the operator UI and `curl` — every tool speaks
JSON. It's the wrong choice for analytics consumers:

- ~10× slower encode/decode than columnar formats for the same
  payload size;
- Loses type fidelity (timestamps round-trip as strings);
- Polars, Pandas, DuckDB, Spark all natively read Arrow IPC but
  have to do schema-inference + parse for NDJSON.

We want operators to pipe ObserveQL output directly into their
analytics stacks without bespoke glue.

## Decision

Content-negotiate Arrow IPC alongside NDJSON. Clients sending
`Accept: application/vnd.apache.arrow.stream` get a
columnar stream; everyone else gets NDJSON.

Implementation:

- `services/query-engine/internal/executor/arrow.go` — new
  `ExecuteArrow` method. Infers schema from the first row, builds
  per-column Arrow builders, writes a single record batch as an
  Arrow IPC stream.
- Type mapping: bool, int*/uint* → int64, float* → float64,
  `time.Time` → timestamp[ns, UTC], everything else → string.
- Empty results are written as a schema-only stream (Arrow IPC
  accepts this).

## Trade-offs

- **Single record batch, not chunked streaming** — the executor
  currently materialises the full result set in memory. Until
  we add a streaming row iterator on the storage client, the
  Arrow encoder mirrors that. Future ADR will add chunked
  RecordBatch streaming once the executor can iterate.
- **Type collapse** — we don't preserve int32 vs int64 vs uint*
  fidelity. The savings of a typed int column over JSON ints
  dominates the small loss in width precision.
- **No dictionary encoding** — Arrow supports dictionary-encoded
  strings (huge wins for low-cardinality columns like
  `tenant_id`, `severity`). We ship a flat encoder first; the
  dictionary opt-in is a one-line change in `inferSchema`.

## Package changes

- `github.com/apache/arrow-go/v18` added to `go.mod`.
- `services/query-engine/internal/executor/arrow.go` — new file.
- `services/query-engine/internal/executor/arrow_test.go` —
  schema inference, round-trip, fallback type.
- `services/query-engine/cmd/main.go` — Accept-header dispatch.

## Wire compatibility

- Default `Content-Type` unchanged (`application/x-ndjson`).
- New `ArrowMediaType = application/vnd.apache.arrow.stream`
  constant exposed from the executor package for clients.

## Verification

- `go test -race ./services/query-engine/...`
- Demo: `curl -H 'Accept: application/vnd.apache.arrow.stream' …`
  pipe into `python -c "import pyarrow.ipc, sys; print(pyarrow.ipc.open_stream(sys.stdin.buffer).read_all())"`.
