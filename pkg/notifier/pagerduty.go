package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PagerDutyNotifier targets PagerDuty Events API v2. The IntegrationKey
// is the "routing key" of a service in PagerDuty (NOT an account-level
// API token); each Service in PD has its own.
//
// Severity mapping:
//
//	SeverityCritical → "critical" (page on-call now)
//	SeverityWarning  → "warning"  (page only if escalation rules say so)
//	SeverityInfo     → "info"     (audit-trail only)
//
// Resolved events use action="resolve" with the same dedup_key, which
// auto-closes the incident in PD.
type PagerDutyNotifier struct {
	IntegrationKey string
	Client         *http.Client
}

func NewPagerDutyNotifier(integrationKey string) *PagerDutyNotifier {
	return &PagerDutyNotifier{
		IntegrationKey: integrationKey,
		Client:         http.DefaultClient,
	}
}

func (p *PagerDutyNotifier) Name() string { return "pagerduty" }

func (p *PagerDutyNotifier) Send(ctx context.Context, n Notification) error {
	if p.IntegrationKey == "" {
		return fmt.Errorf("pagerduty: integration key not configured")
	}
	action := "trigger"
	if n.Resolved() {
		action = "resolve"
	}

	payload := map[string]any{
		"routing_key":  p.IntegrationKey,
		"event_action": action,
		"dedup_key":    n.Fingerprint,
		"payload": map[string]any{
			"summary":  n.Title,
			"source":   n.Source,
			"severity": string(n.Severity),
			"custom_details": map[string]any{
				"tenant_id":   n.TenantID,
				"description": n.Description,
				"labels":      n.Labels,
				"annotations": n.Annotations,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://events.pagerduty.com/v2/enqueue", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		drained, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pagerduty: status %d: %s", resp.StatusCode, string(drained))
	}
	return nil
}
