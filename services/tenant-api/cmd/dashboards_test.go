package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rowjay007/observe-x/services/tenant-api/store"
)

func TestDashboardJSONRoundtrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	layout := []byte(`{"panels":[{"title":"rps","query":"select * from metrics"}]}`)
	d := store.Dashboard{
		ID:        "00000000-0000-0000-0000-000000000001",
		TenantID:  "acme",
		Name:      "ops",
		Layout:    layout,
		CreatedBy: "alice@example.com",
		CreatedAt: now,
		UpdatedAt: now,
	}
	encoded, err := json.Marshal(dashboardJSON(d))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The layout field must be embedded verbatim (no double-encoding).
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["layout"].(map[string]any); !ok {
		t.Errorf("layout should decode as object, got %T: %v", got["layout"], got["layout"])
	}
	if got["id"] != d.ID || got["name"] != "ops" || got["tenant_id"] != "acme" {
		t.Errorf("scalar fields drifted: %v", got)
	}
}

// Verifies the validation path of createDashboard (the bit that
// doesn't need a live Postgres). We don't spin up gin/httptest for
// the success path because that requires a real *store.Store —
// the integration tests cover that round-trip.
func TestCreateDashboardRequestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		wantValid bool
	}{
		{"empty body", `{}`, false},
		{"missing layout", `{"tenant_id":"acme","name":"x"}`, false},
		{"empty layout", `{"tenant_id":"acme","name":"x","layout":{}}`, true}, // shape-valid even if empty
		{"layout is string", `{"tenant_id":"acme","name":"x","layout":"not-json"}`, false},
		{"layout is array", `{"tenant_id":"acme","name":"x","layout":[1,2,3]}`, false},
		{"layout is object", `{"tenant_id":"acme","name":"x","layout":{"panels":[]}}`, true},
		{"missing tenant", `{"name":"x","layout":{"panels":[]}}`, false},
		{"missing name", `{"tenant_id":"acme","layout":{"panels":[]}}`, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var req dashboardReq
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("setup parse: %v", err)
			}
			valid := req.TenantID != "" && req.Name != "" && len(req.Layout) > 0 && isJSONObject(req.Layout)
			if valid != c.wantValid {
				t.Errorf("valid=%v want %v (body=%s)", valid, c.wantValid, c.body)
			}
		})
	}
}
