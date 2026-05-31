package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WebhookNotifier POSTs the full Notification JSON to an arbitrary
// URL. This is the escape hatch for integrations we don't ship a
// dedicated notifier for (Mattermost, Discord, MS Teams, custom
// internal routers, etc.).
//
// The Notification is marshalled directly so consumers see every
// field including Labels/Annotations. Receivers expecting a
// different shape should run their own adapter; the alert-manager
// is not in the schema-translation business.
type WebhookNotifier struct {
	URL     string
	Headers map[string]string
	Client  *http.Client
}

func NewWebhookNotifier(url string, headers map[string]string) *WebhookNotifier {
	return &WebhookNotifier{
		URL:     url,
		Headers: headers,
		Client:  http.DefaultClient,
	}
}

func (w *WebhookNotifier) Name() string { return "webhook" }

func (w *WebhookNotifier) Send(ctx context.Context, n Notification) error {
	if w.URL == "" {
		return fmt.Errorf("webhook: url not configured")
	}
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		drained, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook: status %d: %s", resp.StatusCode, string(drained))
	}
	return nil
}
