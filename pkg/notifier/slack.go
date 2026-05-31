package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SlackNotifier posts to a Slack Incoming Webhook URL. The minimal
// Slack JSON shape — `{"text": "..."}` — is intentional; rich Block
// Kit formatting is Phase D work. The webhook URL is treated as
// sensitive; do not log it.
type SlackNotifier struct {
	WebhookURL string
	Channel    string // optional override
	Client     *http.Client
}

func NewSlackNotifier(webhookURL string) *SlackNotifier {
	return &SlackNotifier{
		WebhookURL: webhookURL,
		Client:     http.DefaultClient,
	}
}

func (s *SlackNotifier) Name() string { return "slack" }

func (s *SlackNotifier) Send(ctx context.Context, n Notification) error {
	if s.WebhookURL == "" {
		return fmt.Errorf("slack: webhook url not configured")
	}
	icon := severityIcon(n.Severity)
	state := "FIRING"
	if n.Resolved() {
		state = "RESOLVED"
		icon = ":white_check_mark:"
	}

	text := fmt.Sprintf("%s *[%s][%s] %s* — tenant `%s`\n%s",
		icon, state, n.Severity, n.Title, n.TenantID, n.Description)

	payload := map[string]any{"text": text}
	if s.Channel != "" {
		payload["channel"] = s.Channel
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		drained, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("slack: status %d: %s", resp.StatusCode, string(drained))
	}
	return nil
}

func severityIcon(s Severity) string {
	switch s {
	case SeverityCritical:
		return ":rotating_light:"
	case SeverityWarning:
		return ":warning:"
	}
	return ":information_source:"
}
