// Package wal is the ObserveX write-ahead log.
//
// Phase A design goals (in priority order — security, correctness,
// maintainability, performance):
//
//  1. Correctness: entries are self-describing (magic + length + crc +
//     timestamp + payload) so recovery can scan a segment and stop at
//     the first corrupt/empty boundary without losing earlier entries.
//
//  2. Crash safety: a background group-commit goroutine fsyncs the
//     active segment every SyncInterval (default 5 ms). This trades a
//     small bounded window of "in-page-cache" entries for batched disk
//     bandwidth — the ObserveX durability promise is "lost within
//     SyncInterval of process/host crash", which is documented and
//     measurable.
//
//  3. Replay: on NewWAL, existing segments are scanned to determine the
//     highest segmentID and the end-of-valid-data offset for the latest
//     segment. Subsequent writes append from there. Earlier data
//     remains readable via Walk.
//
//  4. Hot-path performance: writes are an mmap append + atomic offset
//     bump. No syscall on the critical path until the next group-commit
//     tick.
//
// Entry layout (little endian, total LogHeaderSize=20 bytes + payload):
//
//	+-------+-------+-------+-----------+-----------+
//	| magic | len   | crc32 | timestamp |  payload  |
//	| u32   | u32   | u32   |  int64    |  []byte   |
//	+-------+-------+-------+-----------+-----------+
package wal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// MaxSegmentSize bounds a single segment file. 64 MiB matches the
	// ClickHouse part size sweet-spot and keeps mmap regions modest.
	MaxSegmentSize = 64 * 1024 * 1024

	// LogHeaderSize is magic(4) + length(4) + crc(4) + timestamp(8).
	LogHeaderSize = 20

	// logMagic identifies a valid entry header. The four bytes also let
	// recovery detect zeroed (truncated) tail regions of a segment.
	logMagic uint32 = 0xCAFE0001

	// defaultSyncInterval is the bounded durability window. Anything
	// written within this window before a crash MAY be lost; anything
	// before is guaranteed to be on disk (modulo disk-side caches).
	defaultSyncInterval = 5 * time.Millisecond

	segmentExt = ".log"
)

var (
	// ErrPayloadTooLarge is returned when a single entry would exceed a
	// fresh segment. Callers MUST chunk the payload upstream.
	ErrPayloadTooLarge = errors.New("wal: payload exceeds segment size")

	// ErrClosed is returned by Write after Close.
	ErrClosed = errors.New("wal: closed")
)

// Options configures the WAL. Zero values yield defaults that match the
// values the original Phase 1 code used so existing callers/tests
// continue to work without changes.
type Options struct {
	// SyncInterval bounds how long a write can sit in page cache before
	// fsync. 0 → defaultSyncInterval (5 ms).
	SyncInterval time.Duration

	// MaxSegmentBytes overrides MaxSegmentSize. 0 → MaxSegmentSize.
	MaxSegmentBytes int

	// DisableGroupCommit turns off the background fsync ticker. Tests
	// use this to keep file system traffic deterministic.
	DisableGroupCommit bool
}

func (o Options) withDefaults() Options {
	if o.SyncInterval <= 0 {
		o.SyncInterval = defaultSyncInterval
	}
	if o.MaxSegmentBytes <= 0 {
		o.MaxSegmentBytes = MaxSegmentSize
	}
	return o
}

// WAL is a directory-scoped write-ahead log. It is safe for concurrent
// writes (one writer at a time on the critical section, lock-free
// readers via atomic.Load).
type WAL struct {
	mu sync.Mutex

	dir         string
	opts        Options
	activeFile  *os.File
	mmapData    []byte
	writeOffset int // current append offset in the active mmap
	segmentID   uint64

	dirty  atomic.Bool // unflushed bytes in active segment
	closed atomic.Bool

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewWAL opens a WAL rooted at dir, performing recovery if previous
// segments exist. It does NOT truncate existing data.
func NewWAL(dir string) (*WAL, error) {
	return NewWALWithOptions(dir, Options{})
}

// NewWALWithOptions is the explicit constructor for tests/operators.
func NewWALWithOptions(dir string, opts Options) (*WAL, error) {
	opts = opts.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: mkdir: %w", err)
	}

	w := &WAL{
		dir:    dir,
		opts:   opts,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	segments, err := discoverSegments(dir)
	if err != nil {
		return nil, err
	}

	if len(segments) == 0 {
		if err := w.rotate(); err != nil {
			return nil, err
		}
	} else {
		// Resume into the highest-id segment at its end-of-valid-data.
		last := segments[len(segments)-1]
		if err := w.openSegment(last.id, last.path); err != nil {
			return nil, err
		}
		w.writeOffset = scanValidOffset(w.mmapData)
		w.segmentID = last.id
	}

	if !opts.DisableGroupCommit {
		go w.groupCommitLoop()
	} else {
		close(w.doneCh)
	}
	return w, nil
}

// Write appends a single entry. Returns ErrPayloadTooLarge if the
// entry alone cannot fit in a fresh segment.
func (w *WAL) Write(payload []byte) error {
	if w.closed.Load() {
		return ErrClosed
	}
	entrySize := LogHeaderSize + len(payload)
	if entrySize > w.opts.MaxSegmentBytes {
		return ErrPayloadTooLarge
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.writeOffset+entrySize > w.opts.MaxSegmentBytes {
		if err := w.rotate(); err != nil {
			return err
		}
	}

	off := w.writeOffset
	ts := time.Now().UnixNano()
	crc := crc32.ChecksumIEEE(payload)

	binary.LittleEndian.PutUint32(w.mmapData[off:off+4], logMagic)
	binary.LittleEndian.PutUint32(w.mmapData[off+4:off+8], uint32(len(payload)))
	binary.LittleEndian.PutUint32(w.mmapData[off+8:off+12], crc)
	binary.LittleEndian.PutUint64(w.mmapData[off+12:off+20], uint64(ts))
	copy(w.mmapData[off+LogHeaderSize:off+entrySize], payload)

	w.writeOffset += entrySize
	w.dirty.Store(true)
	return nil
}

// Sync forces an immediate fsync of the active segment. Callers that
// need stronger-than-group-commit durability (e.g. transaction commit
// boundaries) use this; the ingest hot path does not.
func (w *WAL) Sync(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncLocked()
}

// Close fsyncs, unmaps, and closes the active segment. Idempotent.
func (w *WAL) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	if w.stopCh != nil {
		close(w.stopCh)
		<-w.doneCh
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.syncLocked(); err != nil {
		return err
	}
	if err := w.unmapLocked(); err != nil {
		return err
	}
	if w.activeFile != nil {
		return w.activeFile.Close()
	}
	return nil
}

// Walk iterates every persisted entry across every segment, oldest
// first. fn returning a non-nil error halts iteration and the error is
// returned to the caller. Used by Phase B's replay path.
func (w *WAL) Walk(fn func(timestamp time.Time, payload []byte) error) error {
	segments, err := discoverSegments(w.dir)
	if err != nil {
		return err
	}
	for _, seg := range segments {
		if err := walkSegment(seg.path, fn); err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}
			return err
		}
	}
	return nil
}

// ─── internal: segment management ─────────────────────────────────────────

type segmentInfo struct {
	id   uint64
	path string
}

func discoverSegments(dir string) ([]segmentInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: scan dir: %w", err)
	}
	var out []segmentInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segmentExt) {
			continue
		}
		var id uint64
		if _, err := fmt.Sscanf(e.Name(), "%016d.log", &id); err != nil {
			continue
		}
		out = append(out, segmentInfo{id: id, path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// rotate closes the current segment (if any) and opens a fresh one at
// id+1. Caller MUST hold w.mu.
func (w *WAL) rotate() error {
	if w.activeFile != nil {
		if err := w.syncLocked(); err != nil {
			return err
		}
		if err := w.unmapLocked(); err != nil {
			return err
		}
		if err := w.activeFile.Close(); err != nil {
			return err
		}
		w.activeFile = nil
	}

	w.segmentID++
	path := filepath.Join(w.dir, fmt.Sprintf("%016d.log", w.segmentID))
	return w.openSegment(w.segmentID, path)
}

func (w *WAL) openSegment(id uint64, path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("wal: open segment: %w", err)
	}
	if err := f.Truncate(int64(w.opts.MaxSegmentBytes)); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: truncate segment: %w", err)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, w.opts.MaxSegmentBytes,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: mmap: %w", err)
	}
	w.activeFile = f
	w.mmapData = data
	w.writeOffset = 0
	w.segmentID = id
	return nil
}

// syncLocked fsyncs the active file. Caller MUST hold w.mu.
func (w *WAL) syncLocked() error {
	if w.activeFile == nil {
		return nil
	}
	if !w.dirty.Load() {
		return nil
	}
	if err := w.activeFile.Sync(); err != nil {
		return fmt.Errorf("wal: fsync: %w", err)
	}
	w.dirty.Store(false)
	return nil
}

// unmapLocked unmaps the active mmap. Caller MUST hold w.mu.
func (w *WAL) unmapLocked() error {
	if len(w.mmapData) == 0 {
		return nil
	}
	if err := syscall.Munmap(w.mmapData); err != nil {
		return fmt.Errorf("wal: munmap: %w", err)
	}
	w.mmapData = nil
	return nil
}

// groupCommitLoop fsyncs the active segment on every SyncInterval tick
// when there is unflushed data. This bounds the durability window to
// SyncInterval without blocking writers.
func (w *WAL) groupCommitLoop() {
	defer close(w.doneCh)
	t := time.NewTicker(w.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.mu.Lock()
			_ = w.syncLocked()
			w.mu.Unlock()
		case <-w.stopCh:
			return
		}
	}
}

// ─── recovery / iteration ─────────────────────────────────────────────────

// scanValidOffset returns the offset *after* the last valid entry in
// data. Stops at the first invalid magic OR length=0 OR bad CRC. Used
// both by the constructor (resume in active segment) and by Walk.
func scanValidOffset(data []byte) int {
	off := 0
	for off+LogHeaderSize <= len(data) {
		magic := binary.LittleEndian.Uint32(data[off : off+4])
		if magic != logMagic {
			return off
		}
		length := binary.LittleEndian.Uint32(data[off+4 : off+8])
		if length == 0 || off+LogHeaderSize+int(length) > len(data) {
			return off
		}
		crc := binary.LittleEndian.Uint32(data[off+8 : off+12])
		payload := data[off+LogHeaderSize : off+LogHeaderSize+int(length)]
		if crc32.ChecksumIEEE(payload) != crc {
			return off
		}
		off += LogHeaderSize + int(length)
	}
	return off
}

func walkSegment(path string, fn func(time.Time, []byte) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("wal: read segment %s: %w", path, err)
	}
	off := 0
	for off+LogHeaderSize <= len(data) {
		magic := binary.LittleEndian.Uint32(data[off : off+4])
		if magic != logMagic {
			return nil
		}
		length := binary.LittleEndian.Uint32(data[off+4 : off+8])
		if length == 0 || off+LogHeaderSize+int(length) > len(data) {
			return nil
		}
		crc := binary.LittleEndian.Uint32(data[off+8 : off+12])
		ts := int64(binary.LittleEndian.Uint64(data[off+12 : off+20]))
		payload := data[off+LogHeaderSize : off+LogHeaderSize+int(length)]
		if crc32.ChecksumIEEE(payload) != crc {
			return nil
		}
		// Defensive copy so caller can retain the slice.
		buf := make([]byte, len(payload))
		copy(buf, payload)
		if err := fn(time.Unix(0, ts), buf); err != nil {
			return err
		}
		off += LogHeaderSize + int(length)
	}
	return nil
}
