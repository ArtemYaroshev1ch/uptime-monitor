package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// DefaultTelegramURL is the Bot API endpoint.
const DefaultTelegramURL = "https://api.telegram.org"

// TelegramOptions configures a Telegram sender.
type TelegramOptions struct {
	// Token is TELEGRAM_BOT_TOKEN. It is part of every request URL, so it
	// is removed from every error this sender returns.
	Token string
	// BaseURL defaults to DefaultTelegramURL; tests point it at httptest.
	BaseURL string
	// Client defaults to a client without proxy settings from the
	// environment. Requests are bounded by the context.
	Client *http.Client
}

// Telegram sends messages through the Telegram Bot API (sendMessage).
type Telegram struct {
	token   string
	baseURL string
	client  *http.Client
}

// NewTelegram returns a Telegram sender.
func NewTelegram(opts TelegramOptions) *Telegram {
	t := &Telegram{token: opts.Token, baseURL: strings.TrimSuffix(opts.BaseURL, "/"), client: opts.Client}
	if t.baseURL == "" {
		t.baseURL = DefaultTelegramURL
	}
	if t.client == nil {
		t.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	return t
}

type telegramRequest struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
	// Monitor URLs in the text would otherwise get a link preview, which
	// makes Telegram fetch the (possibly down) site.
	DisableWebPagePreview bool `json:"disable_web_page_preview"`
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Send delivers msg to the channel's chat. The text is plain (no
// parse_mode), so monitor names never need escaping.
func (t *Telegram) Send(ctx context.Context, ch domain.Channel, msg domain.Message) error {
	if ch.Config.ChatID == "" {
		return Permanent(errors.New("telegram: channel has no chat_id"))
	}
	body, err := json.Marshal(telegramRequest{ChatID: ch.Config.ChatID, Text: Text(msg), DisableWebPagePreview: true})
	if err != nil {
		return fmt.Errorf("telegram: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/bot"+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return t.redact(fmt.Errorf("telegram: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(fmt.Errorf("telegram: %w", err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return t.redact(fmt.Errorf("telegram: read response: %w", err))
	}
	var tr telegramResponse
	_ = json.Unmarshal(raw, &tr) // a body that is not JSON leaves tr empty
	if resp.StatusCode/100 != 2 || !tr.OK {
		code := resp.StatusCode
		if code/100 == 2 {
			// 200 with "ok": false should not happen; treat it as a
			// server error and retry.
			code = http.StatusBadGateway
		}
		return t.redact(statusError("telegram", code, truncate(tr.Description, maxDetail)))
	}
	return nil
}

// redact removes the bot token from err. Errors from http.Client include the
// request URL (*url.Error), and the token is in its path: without this it
// would end up in logs, in notifications.last_error and in API responses.
// The error chain is kept, so IsPermanent and errors.Is still work.
func (t *Telegram) redact(err error) error {
	if t.token == "" {
		return err
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		ue.URL = strings.ReplaceAll(ue.URL, t.token, "<token>")
	}
	if !strings.Contains(err.Error(), t.token) {
		return err
	}
	// Some other error quotes the token; give up the chain rather than
	// leak it, but keep the permanent mark.
	clean := errors.New(strings.ReplaceAll(err.Error(), t.token, "<token>"))
	if IsPermanent(err) {
		return Permanent(clean)
	}
	return clean
}
