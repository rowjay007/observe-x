package benchmarks

import (
	"os"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/pkg/signal"
	"github.com/rowjay007/observe-x/pkg/wal"
)

func BenchmarkWALWrite(b *testing.B) {
	tempDir, _ := os.MkdirTemp("", "wal-bench-")
	defer os.RemoveAll(tempDir)

	w, _ := wal.NewWAL(tempDir)
	defer w.Close()

	payload := []byte(`{"service":"api","method":"GET","status":200}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		w.Write(payload)
	}
}

func BenchmarkWALWriteParallel(b *testing.B) {
	tempDir, _ := os.MkdirTemp("", "wal-bench-parallel-")
	defer os.RemoveAll(tempDir)

	w, _ := wal.NewWAL(tempDir)
	defer w.Close()

	payload := []byte(`{"service":"api","method":"GET","status":200}`)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w.Write(payload)
		}
	})
}

func BenchmarkSignalChannelThroughput(b *testing.B) {
	signalChan := make(chan signal.Signal, 10000)
	defer close(signalChan)

	payload := []byte(`{"value": 42.5}`)

	sig := signal.Signal{
		TenantID:   "bench-tenant",
		Type:       signal.Metric,
		Payload:    payload,
		Attributes: map[string]string{"service": "api"},
		ReceivedAt: time.Now(),
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		select {
		case signalChan <- sig:
		default:
		}
	}
}

// BenchmarkEstimateThroughput estimates WAL throughput per second
func BenchmarkEstimateThroughput(b *testing.B) {
	tempDir, _ := os.MkdirTemp("", "throughput-bench-")
	defer os.RemoveAll(tempDir)

	w, _ := wal.NewWAL(tempDir)
	defer w.Close()

	payload := []byte(`{"service":"api","endpoint":"/metrics","latency_ms":45}`)

	b.ReportAllocs()

	start := time.Now()
	count := 0

	for time.Since(start) < 1*time.Second && count < 100000 {
		w.Write(payload)
		count++
	}

	elapsed := time.Since(start)
	throughput := float64(count) / elapsed.Seconds()

	b.Logf("Throughput: %.0f signals/sec (target: 12,000/sec)", throughput)

	if throughput >= 12000 {
		b.Logf("✓ Phase 1 throughput requirement MET (%.0f >= 12000)", throughput)
	} else {
		b.Logf("✗ Phase 1 throughput requirement NOT MET (%.0f < 12000)", throughput)
	}
}
