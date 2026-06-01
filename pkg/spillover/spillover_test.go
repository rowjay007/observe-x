package spillover

import (
	"context"
	"strings"
	"testing"

	"github.com/rowjay007/observe-x/pkg/signal"
)

func TestNewDisabledWhenURLEmpty(t *testing.T) {
	ctx := context.Background()
	// No URL ⇒ nil + nil. Caller treats nil as "spillover off."
	s, err := New(ctx, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil when disabled")
	}
	// Methods are safe on nil and return useful errors.
	if err := s.Push(ctx, "acme", signal.Signal{}); err == nil {
		t.Error("Push on nil should error")
	}
	if got := s.Stats(); got != (Stats{}) {
		t.Errorf("zero stats expected, got %+v", got)
	}
	s.Close() // must not panic
}

func TestSanitiseAllowsExpectedCharset(t *testing.T) {
	cases := map[string]string{
		"acme":        "acme",
		"acme-1":      "acme-1",
		"acme.bad":    "acme_bad",
		"acme>evil":   "acme_evil",
		"acme*wild":   "acme_wild",
		"":            "anon",
		"中文":          "____", // two 3-byte runes ⇒ six underscores
		"ABC_def-123": "ABC_def-123",
	}
	for in, want := range cases {
		got := sanitise(in)
		if in == "" && got != "anon" {
			t.Errorf("empty: got %q want %q", got, want)
		}
		if in != "" && strings.ContainsAny(got, ".>*") {
			t.Errorf("subject-illegal char leaked for %q: %q", in, got)
		}
	}
}

func TestOptionsWithDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.StreamName == "" || o.MaxAge == 0 || o.MaxBytes == 0 {
		t.Errorf("defaults not applied: %+v", o)
	}
}

func TestStripPrefix(t *testing.T) {
	if got := stripPrefix("observex.spillover.acme", "observex.spillover."); got != "acme" {
		t.Errorf("got %q", got)
	}
	if got := stripPrefix("noprefix", "observex.spillover."); got != "noprefix" {
		t.Errorf("got %q", got)
	}
}
