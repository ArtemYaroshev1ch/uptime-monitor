package notifier

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

const testToken = "123456:SECRET-bot-token"

var (
	started  = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	resolved = started.Add(5*time.Minute + 30*time.Second)
)

func openedMessage() domain.Message {
	return domain.Message{
		ID:      42,
		Event:   domain.EventIncidentOpened,
		Monitor: domain.MonitorRef{ID: uuid.New(), Name: "Shop", URL: "https://shop.example/"},
		Incident: &domain.Incident{
			ID: uuid.New(), StartedAt: started, CauseKind: domain.ErrorTimeout,
		},
		At: started.Add(time.Minute),
	}
}

// recorder is an httptest handler that stores requests and answers with a
// fixed status and body.
type recorder struct {
	mu     sync.Mutex
	status int
	body   string
	reqs   []recorded
}

type recorded struct {
	path   string
	header http.Header
	body   []byte
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, recorded{path: r.URL.Path, header: r.Header.Clone(), body: body})
	status, resp := rc.status, rc.body
	rc.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (rc *recorder) last(t *testing.T) recorded {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	require.NotEmpty(t, rc.reqs)
	return rc.reqs[len(rc.reqs)-1]
}

func newTelegram(t *testing.T, rc *recorder) *Telegram {
	t.Helper()
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return NewTelegram(TelegramOptions{Token: testToken, BaseURL: srv.URL})
}

func telegramChannel() domain.Channel {
	return domain.Channel{ID: uuid.New(), Type: domain.ChannelTelegram, Name: "me", IsActive: true,
		Config: domain.ChannelConfig{ChatID: "-100123"}}
}

func TestTelegramSend(t *testing.T) {
	t.Parallel()
	rc := &recorder{status: http.StatusOK, body: `{"ok":true,"result":{}}`}
	tg := newTelegram(t, rc)

	require.NoError(t, tg.Send(t.Context(), telegramChannel(), openedMessage()))

	req := rc.last(t)
	assert.Equal(t, "/bot"+testToken+"/sendMessage", req.path)
	assert.Equal(t, "application/json", req.header.Get("Content-Type"))
	var got telegramRequest
	require.NoError(t, json.Unmarshal(req.body, &got))
	assert.Equal(t, "-100123", got.ChatID)
	assert.True(t, got.DisableWebPagePreview)
	assert.Contains(t, got.Text, "DOWN: Shop")
	assert.Contains(t, got.Text, "https://shop.example/")
	assert.Contains(t, got.Text, "Cause: timeout")
	assert.NotContains(t, got.Text, testToken)
}

func TestTelegramErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		body      string
		permanent bool
		contains  string
	}{
		{name: "chat not found", status: 400, body: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, permanent: true, contains: "chat not found"},
		{name: "bot blocked", status: 403, body: `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`, permanent: true, contains: "403"},
		{name: "rate limited", status: 429, body: `{"ok":false,"description":"Too Many Requests: retry after 5"}`, contains: "429"},
		{name: "server error", status: 502, body: `<html>bad gateway</html>`, contains: "502"},
		{name: "ok false", status: 200, body: `{"ok":false}`, contains: "502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tg := newTelegram(t, &recorder{status: tt.status, body: tt.body})

			err := tg.Send(t.Context(), telegramChannel(), openedMessage())

			require.Error(t, err)
			assert.Equal(t, tt.permanent, IsPermanent(err), err)
			assert.Contains(t, err.Error(), tt.contains)
			assert.NotContains(t, err.Error(), testToken)
		})
	}
}

func TestTelegramTransportErrorHidesToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens any more
	tg := NewTelegram(TelegramOptions{Token: testToken, BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := tg.Send(ctx, telegramChannel(), openedMessage())

	require.Error(t, err)
	assert.False(t, IsPermanent(err), "a network error is retried")
	assert.NotContains(t, err.Error(), testToken)
	assert.Contains(t, err.Error(), "<token>")
}

func TestTelegramWithoutChatID(t *testing.T) {
	t.Parallel()
	rc := &recorder{status: http.StatusOK, body: `{"ok":true}`}
	ch := telegramChannel()
	ch.Config.ChatID = ""

	err := newTelegram(t, rc).Send(t.Context(), ch, openedMessage())

	assert.True(t, IsPermanent(err))
	assert.Empty(t, rc.reqs)
}

func webhookChannel(url string) domain.Channel {
	return domain.Channel{ID: uuid.New(), Type: domain.ChannelWebhook, Name: "ops", IsActive: true,
		Config: domain.ChannelConfig{URL: url, Secret: "whsec-test-secret"}}
}

func TestWebhookSendSignsPayload(t *testing.T) {
	t.Parallel()
	rc := &recorder{status: http.StatusNoContent}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	wh := NewWebhook(WebhookOptions{AllowPrivate: true, UserAgent: "uptime-monitor/test"})
	ch := webhookChannel(srv.URL + "/hook")
	msg := openedMessage()
	msg.Event = domain.EventIncidentResolved
	msg.Incident.ResolvedAt = &resolved

	require.NoError(t, wh.Send(t.Context(), ch, msg))

	req := rc.last(t)
	assert.Equal(t, "/hook", req.path)
	assert.Equal(t, Sign("whsec-test-secret", req.body), req.header.Get(SignatureHeader))
	assert.True(t, strings.HasPrefix(req.header.Get(SignatureHeader), "sha256="))
	assert.Equal(t, "incident_resolved", req.header.Get(EventHeader))
	assert.Equal(t, "uptime-monitor/test", req.header.Get("User-Agent"))
	assert.NotContains(t, string(req.body), "whsec-test-secret", "the secret signs the body but is not in it")

	var p map[string]any
	require.NoError(t, json.Unmarshal(req.body, &p))
	assert.EqualValues(t, 42, p["id"])
	assert.Equal(t, "incident_resolved", p["event"])
	assert.Equal(t, "Shop", p["monitor"].(map[string]any)["name"])
	inc := p["incident"].(map[string]any)
	assert.EqualValues(t, 330, inc["duration_seconds"])
	assert.Equal(t, "timeout", inc["cause_kind"])
	assert.NotContains(t, p, "channel")
}

func TestSignKnownValue(t *testing.T) {
	t.Parallel()
	// RFC 4231, test case 2: a receiver verifying with any standard HMAC
	// implementation gets the same value.
	assert.Equal(t,
		"sha256=5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
		Sign("Jefe", []byte("what do ya want for nothing?")))
	assert.NotEqual(t, Sign("secret", []byte("a")), Sign("other", []byte("a")))
}

func TestWebhookErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		permanent bool
	}{
		{name: "server error", status: 500},
		{name: "unavailable", status: 503},
		{name: "rate limited", status: 429},
		{name: "not found", status: 404, permanent: true},
		{name: "redirect is not followed", status: 302, permanent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://elsewhere.example/")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, "nope")
			}))
			t.Cleanup(srv.Close)

			err := NewWebhook(WebhookOptions{AllowPrivate: true}).Send(t.Context(), webhookChannel(srv.URL), openedMessage())

			require.Error(t, err)
			assert.Equal(t, tt.permanent, IsPermanent(err), err)
			assert.NotContains(t, err.Error(), "whsec-test-secret")
		})
	}
}

func TestWebhookBlocksPrivateTargets(t *testing.T) {
	t.Parallel()
	rc := &recorder{status: http.StatusOK}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	wh := NewWebhook(WebhookOptions{AllowPrivate: false})

	for _, url := range []string{srv.URL, "http://169.254.169.254/latest", "ftp://example.com/"} {
		err := wh.Send(t.Context(), webhookChannel(url), openedMessage())

		require.Error(t, err, url)
		assert.True(t, IsPermanent(err), url)
	}
	assert.Empty(t, rc.reqs, "no request reached the internal server")
}

func TestDispatcher(t *testing.T) {
	t.Parallel()
	rc := &recorder{status: http.StatusOK}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	d := Dispatcher{domain.ChannelWebhook: NewWebhook(WebhookOptions{AllowPrivate: true})}

	require.NoError(t, d.Send(t.Context(), webhookChannel(srv.URL), openedMessage()))

	err := d.Send(t.Context(), telegramChannel(), openedMessage())
	assert.True(t, IsPermanent(err))
	assert.Contains(t, err.Error(), "telegram notifications are not configured")
}
