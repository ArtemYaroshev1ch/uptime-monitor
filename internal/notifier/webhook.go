package notifier

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/checker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Webhook delivery headers.
const (
	// SignatureHeader carries "sha256=" + hex(HMAC-SHA256(secret, body)).
	SignatureHeader = "X-Signature"
	// EventHeader carries the event name, so a receiver can route without
	// parsing the body.
	EventHeader = "X-Uptime-Event"
)

// WebhookOptions configures a Webhook sender.
type WebhookOptions struct {
	// AllowPrivate (ALLOW_PRIVATE_TARGETS) permits internal addresses.
	// Webhook URLs come from users, so by default they get the same SSRF
	// protection as monitored URLs.
	AllowPrivate bool
	// UserAgent is sent with every delivery.
	UserAgent string
}

// Webhook POSTs JSON to the channel's URL, signed with the channel secret.
type Webhook struct {
	policy    checker.TargetPolicy
	client    *http.Client
	userAgent string
}

// NewWebhook returns a Webhook sender.
func NewWebhook(opts WebhookOptions) *Webhook {
	dialer := checker.NewSafeDialer(opts.AllowPrivate)
	return &Webhook{
		policy:    checker.NewTargetPolicy(opts.AllowPrivate),
		userAgent: opts.UserAgent,
		client: &http.Client{
			Transport: &http.Transport{
				// No proxy: the dialer must see the receiver's address.
				Proxy:                  nil,
				DialContext:            dialer.DialContext,
				TLSHandshakeTimeout:    10 * time.Second,
				ResponseHeaderTimeout:  10 * time.Second,
				MaxResponseHeaderBytes: 64 << 10,
			},
			// Redirects are not followed: a receiver should be configured
			// with its final URL, and a redirect would re-send a signed
			// body to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Sign returns the X-Signature value of body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Send delivers msg. Any 2xx response is success.
func (w *Webhook) Send(ctx context.Context, ch domain.Channel, msg domain.Message) error {
	if err := w.policy.ValidateURL(ch.Config.URL); err != nil {
		return Permanent(fmt.Errorf("webhook: %w", err))
	}
	body, err := json.Marshal(NewPayload(msg))
	if err != nil {
		return fmt.Errorf("webhook: encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.Config.URL, bytes.NewReader(body))
	if err != nil {
		return Permanent(fmt.Errorf("webhook: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, string(msg.Event))
	req.Header.Set(SignatureHeader, Sign(ch.Config.Secret, body))
	if w.userAgent != "" {
		req.Header.Set("User-Agent", w.userAgent)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		err = fmt.Errorf("webhook: %w", err)
		if checker.IsBlockedConnection(err) {
			return Permanent(err)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		// Drain a little so the server sees a complete exchange; the rest
		// is not needed.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDetail+1))
	return statusError("webhook", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), maxDetail))
}
