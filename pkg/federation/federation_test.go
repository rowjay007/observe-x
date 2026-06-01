package federation

import (
	"context"
	"errors"
	"testing"
)

type fakeBackend struct {
	name string
	rows []map[string]any
	err  error
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Execute(_ context.Context, _ string, _ []any) ([]map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestRouterDispatchesToBoundBackend(t *testing.T) {
	def := &fakeBackend{name: "default", rows: []map[string]any{{"src": "def"}}}
	hot := &fakeBackend{name: "ch", rows: []map[string]any{{"src": "ch"}}}
	r := NewRouter(def)
	r.Register("metrics", hot)

	got, err := r.Execute(context.Background(), "metrics", "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["src"] != "ch" {
		t.Errorf("expected ch, got %v", got[0]["src"])
	}
	// Unbound source falls through to default.
	got2, _ := r.Execute(context.Background(), "unknown", "SELECT 1", nil)
	if got2[0]["src"] != "def" {
		t.Errorf("expected def, got %v", got2[0]["src"])
	}
}

func TestRouterNoBackendErrors(t *testing.T) {
	r := NewRouter(nil)
	_, err := r.Execute(context.Background(), "x", "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected error when no backend bound and no default")
	}
}

func TestExecuteUnionFansOutAndMerges(t *testing.T) {
	hot := &fakeBackend{name: "hot", rows: []map[string]any{
		{"ts": "2025-01-01T10:00:00Z", "from": "hot"},
		{"ts": "2025-01-01T11:00:00Z", "from": "hot"},
	}}
	cold := &fakeBackend{name: "cold", rows: []map[string]any{
		{"ts": "2025-01-01T00:00:00Z", "from": "cold"},
	}}
	r := NewRouter(nil)
	r.Register("hot", hot)
	r.Register("cold", cold)
	got, err := r.ExecuteUnion(context.Background(), []string{"hot", "cold"}, "SELECT *", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: %d", len(got))
	}
	// Sorted ascending by ts ⇒ cold row first.
	if got[0]["from"] != "cold" {
		t.Errorf("first row not the oldest: %v", got[0])
	}
}

func TestExecuteUnionFailFastOnError(t *testing.T) {
	good := &fakeBackend{name: "g", rows: []map[string]any{{}}}
	bad := &fakeBackend{name: "b", err: errors.New("boom")}
	r := NewRouter(nil)
	r.Register("g", good)
	r.Register("b", bad)
	_, err := r.ExecuteUnion(context.Background(), []string{"g", "b"}, "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDuckDBStubReturnsUnsupported(t *testing.T) {
	_, err := NewDuckDBBackend(context.Background(), DuckDBOptions{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}
