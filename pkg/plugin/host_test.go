package plugin

import (
	"context"
	"testing"

	"github.com/rowjay007/observe-x/pkg/plugin/testdata"
)

func TestEnrichSignalRoundTrip(t *testing.T) {
	ctx := context.Background()
	host, err := NewHost(ctx, PluginOptions{})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer func() { _ = host.Close(ctx) }()

	wasm := testdata.BuildEnricherPlugin()
	if _, err := host.Load(ctx, "enricher", wasm); err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := host.EnrichSignal(ctx, "enricher", map[string]any{
		"tenant_id": "acme",
		"severity":  "INFO",
	})
	if err != nil {
		t.Fatalf("EnrichSignal: %v", err)
	}
	if got := out["enriched"]; got != true {
		t.Errorf("expected enriched=true, got %v (full: %+v)", got, out)
	}
	if got := out["source"]; got != "wasm-test" {
		t.Errorf("expected source=wasm-test, got %v", got)
	}
}

func TestLoadRejectsModuleMissingExports(t *testing.T) {
	ctx := context.Background()
	host, err := NewHost(ctx, PluginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close(ctx) }()

	// Minimal module with no exports beyond memory.
	noOpModule := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		// memory section: 1 memory, 1 page no max
		0x05, 0x03, 0x01, 0x00, 0x01,
		// export memory only
		0x07, 0x0a,
		0x01,
		0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
	}
	if _, err := host.Load(ctx, "broken", noOpModule); err == nil {
		t.Fatal("expected error for module missing required exports")
	}
}

func TestEnrichSignalReturnsErrorWhenPluginNotLoaded(t *testing.T) {
	ctx := context.Background()
	host, err := NewHost(ctx, PluginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close(ctx) }()

	if _, err := host.EnrichSignal(ctx, "ghost", map[string]any{}); err == nil {
		t.Fatal("expected error for unloaded plugin")
	}
}

func TestPackUnpackPtrLen(t *testing.T) {
	cases := []struct {
		ptr, length uint32
	}{
		{0, 0},
		{1024, 256},
		{0xFFFFFFFF, 1},
		{1, 0xFFFFFFFF},
	}
	for _, c := range cases {
		ptr, length := UnpackPtrLen(PackPtrLen(c.ptr, c.length))
		if ptr != c.ptr || length != c.length {
			t.Errorf("round-trip lost data: in=(%d,%d) out=(%d,%d)", c.ptr, c.length, ptr, length)
		}
	}
}
