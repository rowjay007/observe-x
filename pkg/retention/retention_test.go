package retention

import (
	"strings"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		wantErr bool
	}{
		{"empty tenant", Spec{}, true},
		{"happy path", Spec{TenantID: "acme", MetricsHotDays: 7, MetricsTotalDays: 30}, false},
		{"hot > total", Spec{TenantID: "acme", LogsHotDays: 30, LogsTotalDays: 10}, true},
		{"negative", Spec{TenantID: "acme", LogsHotDays: -1}, true},
		{"too big", Spec{TenantID: "acme", LogsHotDays: 9999}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate(%+v) err=%v want=%v", c.spec, err, c.wantErr)
			}
		})
	}
}

func TestSafeTenantID(t *testing.T) {
	good := []string{"a", "acme", "ten-1", "tenant_42", strings.Repeat("a", 63)}
	for _, id := range good {
		if err := safeTenantID(id); err != nil {
			t.Errorf("safeTenantID(%q) unexpected err: %v", id, err)
		}
	}
	bad := []string{"", strings.Repeat("a", 64), "ACME", "a;DROP", "a'", "a b", "a/b"}
	for _, id := range bad {
		if err := safeTenantID(id); err == nil {
			t.Errorf("safeTenantID(%q) expected err", id)
		}
	}
}

func TestBuildTTLStatement(t *testing.T) {
	got := buildTTLStatement("metrics", "timestamp", "acme", 30, 90)
	want := "ALTER TABLE metrics MODIFY TTL " +
		"toDateTime(timestamp) + INTERVAL 30 DAY TO DISK 'cold_s3' WHERE tenant_id = 'acme', " +
		"toDateTime(timestamp) + INTERVAL 90 DAY DELETE WHERE tenant_id = 'acme'"
	if got != want {
		t.Errorf("statement mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildTTLStatementHotOnly(t *testing.T) {
	got := buildTTLStatement("logs", "timestamp", "tenant_1", 14, 0)
	if !strings.Contains(got, "TO DISK 'cold_s3'") {
		t.Error("expected hot ALTER")
	}
	if strings.Contains(got, "DELETE") {
		t.Error("did not expect DELETE clause when totalDays=0")
	}
}
