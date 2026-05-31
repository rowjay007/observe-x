//go:build onnx
// +build onnx

package mlruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// onnxPredictor wraps an `onnxruntime` session.
//
// Build with: `go build -tags onnx`.
// Operator MUST:
//   - go get github.com/yalue/onnxruntime_go@latest
//   - install libonnxruntime.{so,dylib,dll} on the binary's LD path
//   - set OBSERVE_X_ML_MODEL_PATH and OBSERVE_X_ML_MODEL_LIB envs
//
// Concurrency: onnxruntime sessions are NOT thread-safe; we serialise
// per-session inference with a mutex. For high QPS, run multiple
// pods rather than reaching for a session pool; the predictor's call
// rate is bounded by the ml-anomaly-detector's HTTP intake.
type onnxPredictor struct {
	opts    OnnxOptions
	session *ort.AdvancedSession
	input   *ort.Tensor[float32]
	output  *ort.Tensor[float32]
	mu      sync.Mutex
}

// NewOnnxPredictor loads an ONNX model and returns a predictor that
// scores Sample.Value as a 1-D float32 input. Models that expect a
// different shape (multi-feature, sliding-window, etc.) need an
// adapter — out of scope for this slice. The model contract is:
//
//   input  shape [1, 1]   dtype float32
//   output shape [1, K]   dtype float32; OutputIndex picks the
//                          column treated as the anomaly score.
//
// Future work: read the contract from the .onnx metadata so
// off-the-shelf sklearn-onnx exports just work without
// per-deployment shape configuration.
func NewOnnxPredictor(ctx context.Context, opts OnnxOptions) (Predictor, error) {
	if opts.ModelPath == "" {
		return nil, errors.New("mlruntime/onnx: ModelPath required")
	}
	if _, err := os.Stat(opts.ModelPath); err != nil {
		return nil, fmt.Errorf("mlruntime/onnx: stat model: %w", err)
	}
	if opts.InputName == "" {
		opts.InputName = "float_input"
	}

	libPath := os.Getenv("OBSERVE_X_ML_MODEL_LIB")
	if libPath != "" {
		ort.SetSharedLibraryPath(libPath)
	}
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("mlruntime/onnx: init: %w", err)
	}

	inputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil {
		return nil, fmt.Errorf("mlruntime/onnx: input tensor: %w", err)
	}
	// We don't know the output shape ahead of time; many models emit
	// shape [1, 1] (raw score) or [1, K] (one-hot probabilities).
	// Start with [1, 1] and let the session detect a mismatch; in
	// that case, operators must adjust their model export.
	outputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil {
		_ = inputTensor.Destroy()
		return nil, fmt.Errorf("mlruntime/onnx: output tensor: %w", err)
	}

	session, err := ort.NewAdvancedSession(opts.ModelPath,
		[]string{opts.InputName}, []string{"score"},
		[]ort.Value{inputTensor}, []ort.Value{outputTensor}, nil)
	if err != nil {
		_ = inputTensor.Destroy()
		_ = outputTensor.Destroy()
		return nil, fmt.Errorf("mlruntime/onnx: session: %w", err)
	}

	p := &onnxPredictor{
		opts:    opts,
		session: session,
		input:   inputTensor,
		output:  outputTensor,
	}
	// Belt-and-suspenders: on GC, ensure the C-side state is freed if
	// the operator forgot to call Close.
	runtime.SetFinalizer(p, func(p *onnxPredictor) { _ = p.Close() })
	return p, nil
}

func (p *onnxPredictor) Name() string { return "onnx:" + p.opts.ModelPath }

func (p *onnxPredictor) Observe(_ context.Context, s Sample) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := p.input.GetData()
	data[0] = float32(s.Value)

	if err := p.session.Run(); err != nil {
		return Decision{}, fmt.Errorf("mlruntime/onnx: run: %w", err)
	}
	out := p.output.GetData()
	idx := p.opts.OutputIndex
	if idx < 0 || idx >= len(out) {
		idx = 0
	}
	score := float64(out[idx])
	d := Decision{Score: score, Threshold: p.opts.ScoreThreshold}
	if score >= p.opts.ScoreThreshold {
		d.Anomaly = true
	}
	return d, nil
}

func (p *onnxPredictor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		_ = p.session.Destroy()
		p.session = nil
	}
	if p.input != nil {
		_ = p.input.Destroy()
		p.input = nil
	}
	if p.output != nil {
		_ = p.output.Destroy()
		p.output = nil
	}
	return nil
}
