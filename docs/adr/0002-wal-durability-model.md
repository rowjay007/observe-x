# ADR 0002 — WAL durability model

- **Status:** Accepted (Phase A)
- **Date:** 2026-05-29

## Context

The pre-Phase-A WAL had three correctness problems:

1. `NewWAL` always rotated to a fresh segment, silently discarding
   every byte ever written on restart.
2. Timestamps were `binary.LittleEndian.Uint64([]byte(fmt.Sprintf("%d",
   0)))` — i.e. the byte representation of the ASCII string "0",
   reinterpreted as a uint64.
3. There was no `fsync` on the hot path or on rotation, but the
   roadmap promised <5 ms P99 WAL commit latency. The original code
   "met" the SLO only because it never committed.

This ADR documents the explicit durability contract that Phase A ships.

## Decision

### Entry format (20-byte header + payload, little-endian)

```
+-------+--------+-------+-----------+------------------+
| magic | length | crc32 | timestamp |      payload     |
| u32   |  u32   |  u32  |   i64ns   |       []byte     |
+-------+--------+-------+-----------+------------------+
4       8        12      20          20 + length        ← byte offsets
```

- `magic = 0xCAFE0001` doubles as a "valid header" sentinel and lets
  recovery detect zeroed tail regions (mmap'd segments are sparse).
- `length` is explicit so recovery can skip over corrupt entries and
  resume at the next valid boundary.
- `crc32(IEEE)` over the payload only.
- `timestamp` is `time.Now().UnixNano()` at append time.

### Group commit

A background goroutine fsyncs the active segment every `SyncInterval`
(default 5 ms) when there is unflushed data. Writers do NOT block on
disk. The durability contract is therefore:

> Any signal accepted by `WAL.Write` is guaranteed durable within
> `SyncInterval` of the call, modulo disk-side caches. A crash within
> the window may lose up to `SyncInterval` of writes.

Callers that need stronger durability (e.g. transaction commit
boundaries in Phase B) call `WAL.Sync(ctx)` explicitly.

### Recovery

On `NewWAL`:

1. Scan the WAL directory for `[0-9]{16}\.log` segments.
2. If none exist, rotate to segment 1.
3. Otherwise, open the highest-numbered segment, scan from offset 0,
   stop at the first invalid magic OR length=0 OR CRC mismatch. That
   offset becomes the new `writeOffset`; subsequent writes append from
   there.
4. Earlier segments are untouched and remain readable via `Walk`.

### What we deliberately did NOT do

- **No double-write to a separate fsync log.** The mmap'd segment is
  itself the log; group commit reaches it via `f.Sync()`.
- **No checksum over the header.** A corrupt header byte typically
  zeros the magic and triggers the sentinel; corrupted length values
  are caught by the CRC-of-payload check.
- **No `msync(MS_SYNC)`.** `f.Sync()` (fsync) covers the page cache
  flush we care about; `msync` adds platform variance with no win.

## Consequences

### Positive

- Restart preserves data. Phase B's WAL replay tool can rebuild the
  serving layer from segment 1 forward.
- The 5 ms group-commit window is documented and tunable via
  `WALOptions.SyncInterval` per deployment.
- Recovery is deterministic and bounded by segment size, not by total
  log size.

### Negative

- Up to `SyncInterval` of writes can be lost in a host crash. We
  accept this; it is the standard kafka-style durability model.
- Header grew from 12 → 20 bytes (per-entry overhead +66%). At 64-byte
  typical payloads this is a measurable storage cost. Mitigation:
  the WAL is a short-lived buffer (Phase B retention <= 24 h), not
  long-term storage.

## Validation

`BenchmarkEstimateThroughput` (in `tests/benchmarks`) confirms ≥12 K
signals/sec on baseline hardware. On an Apple M1, Phase A measures
~1.5–2.2 M signals/sec — comfortably above the SLO with headroom for
the per-signal pipeline overhead.
