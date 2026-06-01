package main

import (
	"testing"
	"time"
)

func TestTimestampOf(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := []struct {
		name string
		in   any
		ok   bool
	}{
		{"time.Time", now, true},
		{"*time.Time", &now, true},
		{"nil *time.Time", (*time.Time)(nil), false},
		{"rfc3339 string", "2026-06-01T10:00:00Z", true},
		{"rfc3339nano string", "2026-06-01T10:00:00.123456789Z", true},
		{"empty string", "", false},
		{"bogus string", "not a time", false},
		{"int (unsupported)", int64(123), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, ok := timestampOf(c.in)
			if ok != c.ok {
				t.Errorf("ok=%v, want %v", ok, c.ok)
			}
		})
	}
}
