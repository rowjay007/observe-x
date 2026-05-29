package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWALRotation(t *testing.T) {
	tempDir := t.TempDir()
	w, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}
	defer w.Close()

	// Calculate a payload size that leaves very little space
	spaceLeftInSegment := MaxSegmentSize - w.writeOffset - LogHeaderSize - 10
	largePayload := make([]byte, spaceLeftInSegment)
	if err := w.Write(largePayload); err != nil {
		t.Errorf("Failed to write large payload: %v", err)
	}

	segmentIDBeforeRotation := w.segmentID

	// Write small payload to trigger rotation
	if err := w.Write([]byte("trigger-rotation")); err != nil {
		t.Errorf("Failed to write after near-full segment: %v", err)
	}

	if w.segmentID <= segmentIDBeforeRotation {
		t.Errorf("Expected segment rotation, but segmentID did not increase: %d -> %d",
			segmentIDBeforeRotation, w.segmentID)
	}

	// Verify segment files were created
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("Failed to read WAL directory: %v", err)
	}

	if len(entries) < 2 {
		t.Errorf("Expected at least 2 segment files, got %d", len(entries))
	}
}

func TestWALEntryRecovery(t *testing.T) {
	tempDir := t.TempDir()
	w, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}

	payload := []byte(`{"test": "data"}`)
	if err := w.Write(payload); err != nil {
		t.Errorf("Failed to write: %v", err)
	}

	w.Close()

	// Reopen and verify data persisted
	w2, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to reopen WAL: %v", err)
	}
	defer w2.Close()

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("Failed to read WAL directory: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("Expected segment file to exist")
	}

	// Verify segment file is readable
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".log" {
			data, err := os.ReadFile(filepath.Join(tempDir, entry.Name()))
			if err != nil {
				t.Errorf("Failed to read segment file: %v", err)
			}

			if len(data) > 0 {
				t.Logf("Segment file '%s' has %d bytes", entry.Name(), len(data))
			}
		}
	}
}

func TestWALConcurrentWrites(t *testing.T) {
	tempDir := t.TempDir()
	w, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}
	defer w.Close()

	numGoroutines := 10
	writesPerGoroutine := 100
	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*writesPerGoroutine)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < writesPerGoroutine; j++ {
				payload := []byte(fmt.Sprintf(`{"goroutine": %d, "index": %d}`, id, j))
				if err := w.Write(payload); err != nil {
					errors <- err
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Errorf("Concurrent write failed: %v", err)
		}
	}
}

func TestWALSyncOnClose(t *testing.T) {
	tempDir := t.TempDir()
	w, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}

	payloads := []string{
		`{"event": 1}`,
		`{"event": 2}`,
		`{"event": 3}`,
	}

	for _, p := range payloads {
		if err := w.Write([]byte(p)); err != nil {
			t.Errorf("Failed to write: %v", err)
		}
	}

	if err := w.Close(); err != nil {
		t.Errorf("Failed to close WAL: %v", err)
	}

	// Verify segment file exists and has data
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("Failed to read WAL directory: %v", err)
	}

	found := false
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".log" {
			info, _ := entry.Info()
			if info.Size() > 0 {
				found = true
				break
			}
		}
	}

	if !found {
		t.Error("Expected segment file with data after close")
	}
}

func TestWALRecoverAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWAL(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	payloads := [][]byte{
		[]byte(`{"event":"alpha"}`),
		[]byte(`{"event":"beta"}`),
		[]byte(`{"event":"gamma"}`),
	}
	for _, p := range payloads {
		if err := w.Write(p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: must NOT rotate — recovery resumes in the same segment.
	w2, err := NewWAL(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()

	if w2.segmentID != 1 {
		t.Fatalf("expected to resume segment 1, got %d", w2.segmentID)
	}

	var got [][]byte
	if err := w2.Walk(func(_ time.Time, p []byte) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != len(payloads) {
		t.Fatalf("expected %d entries on replay, got %d", len(payloads), len(got))
	}
	for i, p := range payloads {
		if string(got[i]) != string(p) {
			t.Errorf("entry %d: want %q got %q", i, p, got[i])
		}
	}
}

func TestWALSegmentBoundary(t *testing.T) {
	tempDir := t.TempDir()
	w, err := NewWAL(tempDir)
	if err != nil {
		t.Fatalf("Failed to create WAL: %v", err)
	}
	defer w.Close()

	smallPayload := []byte(`{"small": "payload"}`)
	largePayload := make([]byte, MaxSegmentSize-w.writeOffset-LogHeaderSize-10)

	if err := w.Write(smallPayload); err != nil {
		t.Errorf("Failed to write small payload: %v", err)
	}

	initialSegmentID := w.segmentID

	if err := w.Write(largePayload); err != nil {
		t.Errorf("Failed to write large payload: %v", err)
	}

	if w.segmentID == initialSegmentID {
		t.Error("Expected rotation to occur at segment boundary")
	}
}
