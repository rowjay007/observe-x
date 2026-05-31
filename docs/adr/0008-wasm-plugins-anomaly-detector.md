# ADR 0008 — WASM plugin host + anomaly-detector skeleton

- **Status:** Accepted (Phase B-5)
- **Date:** 2026-05-29

## Context

Two roadmap items both fit "we have an extensibility story and a
first ML pass" without expanding into multi-week new-product work:

1. **Tenant-supplied plugins.** Big customers will eventually want
   to enrich, redact, or transform their signals before storage. We
   need a sandbox model that runs untrusted code safely.
2. **First-pass anomaly detection.** Roadmap Phase 4 has a full
   anomaly-detection service. Phase B-5 ships a skeleton with a
   real (if simple) detection algorithm, so the wire format and
   service boundary are settled before the ML work starts.

## Decision

### Plugin host: wazero, not wasmtime

`pkg/plugin` is built on `github.com/tetratelabs/wazero`. Why not
`wasmtime-go`:

- wazero is **pure Go** — no CGo, no shared library, no glibc-vs-musl
  pain in Alpine containers.
- The runtime profile for an enrich-signal plugin (small,
  short-lived calls, no JIT warm-up window worth optimising) makes
  the JIT advantage of wasmtime irrelevant.
- The deploy story for a Go binary that bundles wasmtime is "ship a
  .so alongside" or "static-link with CGo enabled and a custom toolchain."
  Both are operational tax we don't need to pay.

If a future plugin profile changes (e.g. long-running ML inference
with hot loops), the seam is `Host.EnrichSignal`; the runtime
swap is a re-implementation of that file, not a rewrite of the
plugin contract.

### ABI: shared-memory, JSON in/out

The plugin contract is intentionally JSON-shaped for Phase B-5:

```
plugin exports:
  memory                              required, page-capped
  alloc(size) -> ptr                  host writes input here
  free(ptr, len)                      host calls after the round trip
  enrich_signal(in_ptr, in_len) -> i64  high32=out_ptr, low32=out_len
                                       (0,0) ⇒ no-op passthrough

host imports (env namespace):
  log_info(ptr, len)
  metric_inc(name_ptr, name_len)
  now_nanos() -> i64
```

JSON not protobuf because:
- Every WASM language can encode JSON; not every language has a
  good protobuf compiler.
- Plugin authors aren't going to be inspecting plugin behaviour by
  decoding protobuf with hexdump.
- Per-signal protobuf encoding is overkill for a plugin call that
  is already on the slow path relative to native enrichment.

When (if) the plugin path becomes hot enough to matter, swap
encoding to FlatBuffers behind the same Go-level Signal type.

### Sandbox limits

- Memory cap: 16 MiB by default (256 × 64 KiB pages), enforced by
  wazero's `WithMemoryLimitPages`.
- Per-call deadline: 50 ms by default, enforced by
  `context.WithTimeout`. wazero with `WithCloseOnContextDone(true)`
  terminates the module on deadline.
- Capability deny-by-default: no `fs`, no `random`, no `clock`
  beyond the `now_nanos` host import. Plugin authors who need a
  capability ask for it; we add a host function.

### Anomaly detector: rolling z-score, not Prophet

`services/ml-anomaly-detector` is a separate deployable that ingests
`POST /v1/observations` (in Phase C: subscribes to JetStream). The
detection algorithm is Welford-style EWMA mean+variance per
(tenant, metric) series, with anomalies firing at |z| > 3 (default)
after 50 warmup samples. Why this and not Prophet/STL:

- The wire format and service boundary settle today. When the
  algorithm becomes the differentiator, the swap is local.
- z-score works for a startling fraction of real anomaly cases —
  it's the baseline every other algorithm beats by single-digit
  percentage points.
- Prophet ships Python; we'd have to make a CGo + libpython decision
  to host it in-process, or stand up a sidecar. Out of scope for
  Phase B.

The detector preserves per-(tenant, metric) isolation: two tenants
emitting the same metric name learn separate baselines. Verified
by `TestDetectorIsolatesSeriesAcrossTenants`.

## Trade-offs

**JSON ABI is slow.** Encoding/decoding JSON twice per signal puts a
~5-10µs floor on plugin-enriched signals vs. zero-copy native
enrichment. Acceptable because plugins are opt-in per tenant and
the alternative (any plugin failure mode taking down the gateway)
is unacceptable.

**Skeleton anomaly detector ≠ production.** The detector ships with
no persistence, no NATS, no alert escalation. The intent is to
freeze the wire contract and prove the detection-loop architecture;
the actual ML is Phase C.

**Hand-encoded test WASM.** The unit test builds its own .wasm bytes
at test time (`pkg/plugin/testdata/builder.go`) instead of committing
a binary. Slower to evolve (encode a new module by hand) but
keeps the repo free of opaque binaries.

## Package changes

| Package                                     | Change |
|---------------------------------------------|--------|
| `pkg/plugin/`                               | NEW. wazero host + ABI primitives. |
| `pkg/plugin/testdata/`                      | NEW. WASM-binary builder for the test plugin. |
| `services/ml-anomaly-detector/`             | NEW service. HTTP /v1/observations + rolling-z detector. |
| `github.com/tetratelabs/wazero`             | NEW dep. |

No existing package was modified.
