//go:build !onnx
// +build !onnx

package mlruntime

import "context"

// NewOnnxPredictor in the default build returns ErrUnsupported so
// operators get a clear error at startup if they ask for ONNX without
// compiling with `-tags onnx`. The signature matches the tagged
// implementation so consuming code doesn't need build-tag awareness.
//
// The tagged implementation requires `github.com/yalue/onnxruntime_go`
// (CGo) plus the libonnxruntime shared library shipped by the
// operator at OBSERVE_X_ML_MODEL_PATH-adjacent paths. See
// docs/adr/0016-ml-runtime.md for the full bring-your-own-model
// contract.
func NewOnnxPredictor(_ context.Context, _ OnnxOptions) (Predictor, error) {
	return nil, ErrUnsupported
}

// OnnxOptions is the configuration surface, mirrored in both build
// variants so consuming code stays portable.
type OnnxOptions struct {
	// ModelPath is the on-disk path of the .onnx file.
	ModelPath string
	// InputName is the model's input tensor name (defaults vary by
	// exporter; sklearn-onnx commonly emits "float_input").
	InputName string
	// InputFeatures is the number of float32s the model expects.
	// Defaults to 1 (single-feature back-compat). Phase D-1: models
	// with >1 feature read Sample.Features rather than Sample.Value.
	InputFeatures int
	// OutputIndex selects which output tensor contains the anomaly
	// score (defaults to 0).
	OutputIndex int
	// ScoreThreshold above which a sample is anomalous. The
	// semantics depend on the model — Isolation Forest commonly
	// uses 0.5 as the boundary for "outlier."
	ScoreThreshold float64
}
