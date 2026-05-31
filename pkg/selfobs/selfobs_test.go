package selfobs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestInitWithoutEndpointReturnsNoop(t *testing.T) {
	p, err := Init(context.Background(), Config{ServiceName: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.tp.(noop.TracerProvider); !ok {
		t.Errorf("expected noop provider when endpoint is empty, got %T", p.tp)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown should not error: %v", err)
	}
}

func TestInitRequiresServiceName(t *testing.T) {
	if _, err := Init(context.Background(), Config{}); err == nil {
		t.Error("expected error for missing ServiceName")
	}
}

func TestStripScheme(t *testing.T) {
	cases := map[string]string{
		"http://x:7000":   "x:7000",
		"https://y:7000":  "y:7000",
		"plain.host:9000": "plain.host:9000",
	}
	for in, want := range cases {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
