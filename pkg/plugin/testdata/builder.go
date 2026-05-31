// Package testdata builds the minimal valid WebAssembly module used by
// pkg/plugin/host_test.go. We generate the bytes in Go so the test is
// self-contained (no wat2wasm dependency, no committed binary blob).
//
// The module exports:
//   - memory (1 page)
//   - alloc(size i32) -> i32      bump allocator from offset 4096
//   - free(ptr i32, len i32) -> () no-op
//   - enrich_signal(inPtr, inLen i32) -> i64
//        returns the packed (out_ptr=0, out_len=N) where the
//        output JSON `{"enriched":true,"source":"wasm-test"}` lives
//        in a data segment at offset 0.
package testdata

import (
	"bytes"
	"encoding/binary"
)

// BuildEnricherPlugin returns the WASM binary bytes for a plugin that
// ignores its input and always returns the fixed JSON output.
func BuildEnricherPlugin() []byte {
	const output = `{"enriched":true,"source":"wasm-test"}`
	outLen := uint32(len(output))

	var b bytes.Buffer

	// Magic + version
	b.Write([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})

	// ── Section 1: Types ──
	// type 0: (i32) -> i32           alloc
	// type 1: (i32, i32) -> ()       free
	// type 2: (i32, i32) -> i64      enrich_signal
	types := concat(
		[]byte{0x03}, // count
		[]byte{0x60, 0x01, 0x7f, 0x01, 0x7f},
		[]byte{0x60, 0x02, 0x7f, 0x7f, 0x00},
		[]byte{0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e},
	)
	writeSection(&b, 0x01, types)

	// ── Section 3: Functions (typeidx per func) ──
	writeSection(&b, 0x03, []byte{0x03, 0x00, 0x01, 0x02})

	// ── Section 5: Memory (1 page, no max) ──
	writeSection(&b, 0x05, []byte{0x01, 0x00, 0x01})

	// ── Section 6: Globals (mut i32 = 4096) ──
	writeSection(&b, 0x06, []byte{
		0x01,                         // count
		0x7f, 0x01,                   // i32 mut
		0x41, 0x80, 0x20, 0x0b,       // i32.const 4096, end (LEB128: 4096 = 0x80 0x20)
	})

	// ── Section 7: Exports ──
	exports := concat(
		[]byte{0x04}, // count
		exportEntry("memory", 0x02, 0),
		exportEntry("alloc", 0x00, 0),
		exportEntry("free", 0x00, 1),
		exportEntry("enrich_signal", 0x00, 2),
	)
	writeSection(&b, 0x07, exports)

	// ── Section 10: Code ──
	// alloc body
	allocBody := []byte{
		0x01, 0x01, 0x7f,             // 1 local group: 1 i32 (the $ptr)
		0x23, 0x00,                   // global.get 0
		0x21, 0x01,                   // local.set 1 ($ptr)
		0x23, 0x00,                   // global.get 0
		0x20, 0x00,                   // local.get 0 ($size)
		0x6a,                         // i32.add
		0x24, 0x00,                   // global.set 0
		0x20, 0x01,                   // local.get 1
		0x0b,                         // end
	}
	// free body: just end
	freeBody := []byte{0x00, 0x0b}
	// enrich body: i64.const outLen, end (high32=0 → ptr 0; low32=outLen)
	enrichBody := concat([]byte{0x00, 0x42}, encodeSLEB128(int64(outLen)), []byte{0x0b})

	code := concat(
		[]byte{0x03}, // count
		prependLen(allocBody),
		prependLen(freeBody),
		prependLen(enrichBody),
	)
	writeSection(&b, 0x0a, code)

	// ── Section 11: Data ──
	dataSeg := concat(
		[]byte{0x01},                 // count
		[]byte{0x00},                 // segment type 0 (active, mem 0)
		[]byte{0x41, 0x00, 0x0b},     // offset = i32.const 0, end
		encodeULEB128(uint64(outLen)),
		[]byte(output),
	)
	writeSection(&b, 0x0b, dataSeg)

	return b.Bytes()
}

// ─── WASM encoding helpers ────────────────────────────────────────────────

func writeSection(b *bytes.Buffer, id byte, payload []byte) {
	b.WriteByte(id)
	b.Write(encodeULEB128(uint64(len(payload))))
	b.Write(payload)
}

func exportEntry(name string, kind byte, idx uint32) []byte {
	out := concat([]byte{byte(len(name))}, []byte(name), []byte{kind})
	return concat(out, encodeULEB128(uint64(idx)))
}

func prependLen(body []byte) []byte {
	return append(encodeULEB128(uint64(len(body))), body...)
}

func concat(parts ...[]byte) []byte {
	var total int
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func encodeULEB128(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			break
		}
	}
	return out
}

func encodeSLEB128(v int64) []byte {
	var out []byte
	more := true
	for more {
		b := byte(v & 0x7f)
		v >>= 7
		signBit := b & 0x40
		if (v == 0 && signBit == 0) || (v == -1 && signBit != 0) {
			more = false
		} else {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// keep binary imported in case we add fixed-width int encoders later
var _ = binary.LittleEndian
