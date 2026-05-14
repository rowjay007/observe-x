package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	MaxSegmentSize = 64 * 1024 * 1024
	LogHeaderSize  = 12
)

type LogEntry struct {
	CRC       uint32
	Timestamp int64
	Payload   []byte
}

type WAL struct {
	mu          sync.RWMutex
	dir         string
	activeFile  *os.File
	mmapData    []byte
	writeOffset int
	segmentID   uint64
}

func NewWAL(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	w := &WAL{dir: dir}
	if err := w.rotate(); err != nil {
		return nil, err
	}

	return w, nil
}

func (w *WAL) rotate() error {
	if w.activeFile != nil {
		_ = w.sync()
		_ = w.unmap()
		w.activeFile.Close()
	}

	w.segmentID++
	path := filepath.Join(w.dir, fmt.Sprintf("%016d.log", w.segmentID))

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if err := f.Truncate(MaxSegmentSize); err != nil {
		f.Close()
		return err
	}

	data, err := syscall.Mmap(int(f.Fd()), 0, MaxSegmentSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return err
	}

	w.activeFile = f
	w.mmapData = data
	w.writeOffset = 0
	return nil
}

func (w *WAL) Write(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	entrySize := LogHeaderSize + len(payload)
	if w.writeOffset+entrySize > MaxSegmentSize {
		if err := w.rotate(); err != nil {
			return err
		}
	}

	ts := time.Now().UnixNano()
	crc := crc32.ChecksumIEEE(payload)

	if w.writeOffset+LogHeaderSize > MaxSegmentSize {
		return fmt.Errorf("not enough space for header")
	}

	if w.writeOffset+LogHeaderSize+len(payload) > MaxSegmentSize {
		return fmt.Errorf("not enough space for payload")
	}

	binary.LittleEndian.PutUint32(w.mmapData[w.writeOffset:w.writeOffset+4], crc)
	binary.LittleEndian.PutUint64(w.mmapData[w.writeOffset+4:w.writeOffset+12], uint64(ts))
	copy(w.mmapData[w.writeOffset+12:w.writeOffset+entrySize], payload)

	w.writeOffset += entrySize
	return nil
}

func (w *WAL) sync() error {
	if w.activeFile == nil {
		return nil
	}
	return w.activeFile.Sync()
}

func (w *WAL) unmap() error {
	if len(w.mmapData) == 0 {
		return nil
	}
	return syscall.Munmap(w.mmapData)
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sync()
	w.unmap()
	return w.activeFile.Close()
}
