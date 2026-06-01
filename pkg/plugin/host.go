// Package plugin hosts tenant-supplied WebAssembly modules that can
// observe and enrich signals as they flow through the pipeline. The
// host is built on wazero, a pure-Go WASM runtime — no CGo, no
// dynamic libraries.
//
// ABI contract (Phase B-5):
//
//	Plugin exports:
//	  (export "memory" (memory ...))                      — required, page-size capped
//	  (export "alloc" (func (param i32) (result i32)))    — host calls to write input
//	  (export "free" (func (param i32 i32)))              — host calls to release input
//	  (export "enrich_signal"
//	     (func (param i32 i32) (result i64)))             — main entrypoint
//
//	enrich_signal takes (ptr, len) of an input JSON document and
//	returns a packed i64: high 32 bits = output ptr, low 32 bits =
//	output len. The output JSON is read from the guest's memory.
//	Returning (0, 0) means "no enrichment, pass through".
//
//	Host imports (env namespace):
//	  (import "env" "log_info"    (func (param i32 i32)))
//	  (import "env" "metric_inc"  (func (param i32 i32)))
//	  (import "env" "now_nanos"   (func (result i64)))
//
// Resource limits:
//   - Memory cap (default 16 MiB) enforced by wazero's RuntimeConfig.
//   - Per-invocation deadline via context.WithTimeout.
//   - No filesystem, no network, no environment variables — wazero
//     starts with a default-deny capability set.
//
// See docs/adr/0008-wasm-plugins-anomaly-detector.md.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// PluginOptions configures the WASM runtime.
type PluginOptions struct {
	// MemoryPages caps the linear memory in 64 KiB pages.
	// Default 256 (16 MiB).
	MemoryPages uint32
	// CallTimeout bounds a single enrich_signal invocation.
	// Default 50ms.
	CallTimeout time.Duration
}

func (o PluginOptions) withDefaults() PluginOptions {
	if o.MemoryPages == 0 {
		o.MemoryPages = 256
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 50 * time.Millisecond
	}
	return o
}

// Host owns one wazero runtime that compiled plugins are loaded into.
// Loaded plugins are referenced by name; the host is safe for
// concurrent EnrichSignal calls across plugins.
type Host struct {
	opts    PluginOptions
	rt      wazero.Runtime
	envMod  api.Module
	mu      sync.RWMutex
	plugins map[string]*Plugin

	logHook    func(plugin, msg string)
	metricHook func(plugin, name string)
}

// HostOption mutates Host construction. Used for test hooks.
type HostOption func(*Host)

func WithLogHook(fn func(plugin, msg string)) HostOption {
	return func(h *Host) { h.logHook = fn }
}
func WithMetricHook(fn func(plugin, name string)) HostOption {
	return func(h *Host) { h.metricHook = fn }
}

// NewHost constructs a Host. Call Close to release all loaded modules.
func NewHost(ctx context.Context, opts PluginOptions, hostOpts ...HostOption) (*Host, error) {
	opts = opts.withDefaults()

	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(opts.MemoryPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)

	h := &Host{
		opts:    opts,
		rt:      rt,
		plugins: map[string]*Plugin{},
	}
	for _, o := range hostOpts {
		o(h)
	}
	if err := h.registerEnvModule(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	return h, nil
}

func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.plugins {
		_ = p.module.Close(ctx)
	}
	h.plugins = nil
	return h.rt.Close(ctx)
}

// ─── env-namespace host functions ────────────────────────────────────────

// pluginNameKey is a context.WithValue key for stamping every host
// callback with the calling plugin's name.
type pluginNameKey struct{}

func (h *Host) registerEnvModule(ctx context.Context) error {
	b := h.rt.NewHostModuleBuilder("env")

	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, ptr, length uint32) {
		if h.logHook == nil {
			return
		}
		mem := m.Memory()
		buf, ok := mem.Read(ptr, length)
		if !ok {
			return
		}
		h.logHook(pluginName(ctx), string(buf))
	}).Export("log_info")

	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, namePtr, nameLen uint32) {
		if h.metricHook == nil {
			return
		}
		mem := m.Memory()
		buf, ok := mem.Read(namePtr, nameLen)
		if !ok {
			return
		}
		h.metricHook(pluginName(ctx), string(buf))
	}).Export("metric_inc")

	b.NewFunctionBuilder().WithFunc(func(_ context.Context) int64 {
		return time.Now().UnixNano()
	}).Export("now_nanos")

	mod, err := b.Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("plugin: register env module: %w", err)
	}
	h.envMod = mod
	return nil
}

func pluginName(ctx context.Context) string {
	v, _ := ctx.Value(pluginNameKey{}).(string)
	if v == "" {
		return "unknown"
	}
	return v
}

// ─── plugin loading ──────────────────────────────────────────────────────

// Plugin is a single loaded WASM module with the four required exports.
type Plugin struct {
	name   string
	module api.Module
	alloc  api.Function
	freeFn api.Function
	enrich api.Function
	memory api.Memory
}

// Load compiles wasmBytes and instantiates it with the env module
// already linked. The plugin is registered under name; subsequent
// Load calls with the same name replace the previous instance.
func (h *Host) Load(ctx context.Context, name string, wasmBytes []byte) (*Plugin, error) {
	if name == "" {
		return nil, errors.New("plugin: name required")
	}
	compiled, err := h.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("plugin: compile %s: %w", name, err)
	}
	mod, err := h.rt.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName(name))
	if err != nil {
		return nil, fmt.Errorf("plugin: instantiate %s: %w", name, err)
	}

	p := &Plugin{
		name:   name,
		module: mod,
		memory: mod.Memory(),
		alloc:  mod.ExportedFunction("alloc"),
		freeFn: mod.ExportedFunction("free"),
		enrich: mod.ExportedFunction("enrich_signal"),
	}
	if p.alloc == nil || p.freeFn == nil || p.enrich == nil || p.memory == nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("plugin %s: missing required exports (alloc/free/enrich_signal/memory)", name)
	}

	h.mu.Lock()
	if prev, ok := h.plugins[name]; ok {
		_ = prev.module.Close(ctx)
	}
	h.plugins[name] = p
	h.mu.Unlock()

	return p, nil
}

// Get returns a loaded plugin by name, or nil if absent.
func (h *Host) Get(name string) *Plugin {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.plugins[name]
}

// ─── EnrichSignal — the hot path ─────────────────────────────────────────

// EnrichSignal serialises input as JSON, hands it to the plugin's
// enrich_signal, and returns the plugin's output JSON (or input
// unchanged if the plugin returned (0,0)).
//
// The call is bounded by Host.CallTimeout. Any plugin trap, memory
// fault, or timeout is returned as an error; the original input is
// preserved by the caller (we never mutate input).
func (h *Host) EnrichSignal(ctx context.Context, pluginName string, input map[string]any) (map[string]any, error) {
	p := h.Get(pluginName)
	if p == nil {
		return nil, fmt.Errorf("plugin %s: not loaded", pluginName)
	}
	inJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("plugin: marshal input: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, h.opts.CallTimeout)
	callCtx = context.WithValue(callCtx, pluginNameKey{}, pluginName)
	defer cancel()

	// Allocate guest memory for the input.
	allocRes, err := p.alloc.Call(callCtx, uint64(len(inJSON)))
	if err != nil {
		return nil, fmt.Errorf("plugin %s: alloc: %w", pluginName, err)
	}
	inPtr := uint32(allocRes[0])
	defer func() {
		_, _ = p.freeFn.Call(context.Background(), uint64(inPtr), uint64(len(inJSON)))
	}()

	if !p.memory.Write(inPtr, inJSON) {
		return nil, fmt.Errorf("plugin %s: write input failed", pluginName)
	}

	res, err := p.enrich.Call(callCtx, uint64(inPtr), uint64(len(inJSON)))
	if err != nil {
		return nil, fmt.Errorf("plugin %s: enrich: %w", pluginName, err)
	}
	packed := res[0]
	outPtr := uint32(packed >> 32)
	outLen := uint32(packed & 0xFFFF_FFFF)
	if outPtr == 0 && outLen == 0 {
		return input, nil // passthrough
	}
	outBytes, ok := p.memory.Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("plugin %s: read output failed", pluginName)
	}
	defer func() {
		_, _ = p.freeFn.Call(context.Background(), uint64(outPtr), uint64(outLen))
	}()

	out := map[string]any{}
	if err := json.Unmarshal(outBytes, &out); err != nil {
		return nil, fmt.Errorf("plugin %s: invalid output JSON: %w", pluginName, err)
	}
	return out, nil
}

// PackPtrLen encodes (ptr, len) into the i64 layout that the
// enrich_signal export uses for its return value (high32=ptr, low32=len).
func PackPtrLen(ptr, length uint32) int64 {
	return int64((uint64(ptr) << 32) | uint64(length))
}

func UnpackPtrLen(packed int64) (ptr, length uint32) {
	u := uint64(packed)
	return uint32(u >> 32), uint32(u & 0xFFFF_FFFF)
}
