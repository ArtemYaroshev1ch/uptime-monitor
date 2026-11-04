package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// recordingSender stands in for notifier.Dispatcher in HTTP tests.
type recordingSender struct {
	mu   sync.Mutex
	err  error
	sent []domain.Channel
}

func (s *recordingSender) Send(_ context.Context, ch domain.Channel, _ domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, ch)
	return s.err
}

func (s *recordingSender) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

type channelJSON struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Config struct {
		ChatID string `json:"chat_id"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
	} `json:"config"`
	IsActive bool `json:"is_active"`
}

func (a testAPI) createChannel(t *testing.T, token, body string) channelJSON {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/api/v1/channels", body, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return decode[channelJSON](t, rec.Body.Bytes())
}

func TestChannelsCRUD(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	token := api.userToken(t, "alice@example.com")

	rec := api.do(t, http.MethodPost, "/api/v1/channels",
		`{"type":"telegram","name":"me","config":{"chat_id":"-100123"}}`, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	tg := decode[channelJSON](t, rec.Body.Bytes())
	assert.Equal(t, "/api/v1/channels/"+tg.ID, rec.Header().Get("Location"))
	assert.JSONEq(t, `{
		"id": "`+tg.ID+`", "type": "telegram", "name": "me",
		"config": {"chat_id": "-100123"}, "is_active": true,
		"created_at": "2026-01-02T03:04:05Z"
	}`, rec.Body.String())

	// The webhook secret is shown once, in the create response.
	rec = api.do(t, http.MethodPost, "/api/v1/channels",
		`{"type":"webhook","name":"ops","config":{"url":"https://hooks.example/x"}}`, token)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	wh := decode[channelJSON](t, rec.Body.Bytes())
	require.NotEmpty(t, wh.Config.Secret)
	secret := wh.Config.Secret

	for _, path := range []string{"/api/v1/channels", "/api/v1/channels/" + wh.ID} {
		rec = api.do(t, http.MethodGet, path, "", token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), secret, path)
		assert.NotContains(t, rec.Body.String(), `"secret"`, path)
	}
	rec = api.do(t, http.MethodGet, "/api/v1/channels", "", token)
	list := decode[listJSON[channelJSON]](t, rec.Body.Bytes())
	assert.Equal(t, 2, list.Total)
	require.Len(t, list.Items, 2)
	assert.Equal(t, "ops", list.Items[0].Name, "newest first")

	rec = api.do(t, http.MethodPatch, "/api/v1/channels/"+wh.ID, `{"is_active":false,"config":{"url":"https://hooks.example/y"}}`, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	updated := decode[channelJSON](t, rec.Body.Bytes())
	assert.False(t, updated.IsActive)
	assert.Equal(t, "https://hooks.example/y", updated.Config.URL)
	assert.NotContains(t, rec.Body.String(), secret)

	rec = api.do(t, http.MethodDelete, "/api/v1/channels/"+tg.ID, "", token)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = api.do(t, http.MethodGet, "/api/v1/channels/"+tg.ID, "", token)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestChannelsValidation(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	token := api.userToken(t, "alice@example.com")
	wh := api.createChannel(t, token, `{"type":"webhook","name":"ops","config":{"url":"https://hooks.example/x"}}`)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		want   string
	}{
		{
			name: "private webhook", method: http.MethodPost, path: "/api/v1/channels",
			body: `{"type":"webhook","name":"x","config":{"url":"http://127.0.0.1:8080/"}}`,
			want: `"field":"config.url"`,
		},
		{
			name: "secret is not an input", method: http.MethodPost, path: "/api/v1/channels",
			body: `{"type":"webhook","name":"x","config":{"url":"https://h.example","secret":"mine"}}`,
			want: `"field":"secret","issue":"is not a known field"`,
		},
		{
			name: "type cannot change", method: http.MethodPatch, path: "/api/v1/channels/" + wh.ID,
			body: `{"type":"telegram"}`,
			want: `"field":"type","issue":"cannot be changed"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.do(t, tt.method, tt.path, tt.body, token)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), `"code":"validation_failed"`)
			assert.Contains(t, rec.Body.String(), tt.want)
		})
	}
}

func TestChannelsLimit(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	token := api.userToken(t, "alice@example.com")
	for range 10 {
		api.createChannel(t, token, `{"type":"telegram","name":"x","config":{"chat_id":"1"}}`)
	}

	rec := api.do(t, http.MethodPost, "/api/v1/channels", `{"type":"telegram","name":"x","config":{"chat_id":"1"}}`, token)

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestChannelTest(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	token := api.userToken(t, "alice@example.com")
	tg := api.createChannel(t, token, `{"type":"telegram","name":"me","config":{"chat_id":"1"}}`)

	rec := api.do(t, http.MethodPost, "/api/v1/channels/"+tg.ID+"/test", "", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"delivered": true}`, rec.Body.String())
	assert.Equal(t, 1, api.sender.count())

	api.sender.failWith(errors.New("telegram responded 400 Bad Request: chat not found"))
	rec = api.do(t, http.MethodPost, "/api/v1/channels/"+tg.ID+"/test", "", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"delivered": false, "error": "telegram responded 400 Bad Request: chat not found"}`, rec.Body.String())
}

// TestChannelsIsolation: another user's channel is a 404 for every route,
// and nothing is sent to it.
func TestChannelsIsolation(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	alice := api.userToken(t, "alice@example.com")
	bob := api.userToken(t, "bob@example.com")
	ch := api.createChannel(t, alice, `{"type":"webhook","name":"ops","config":{"url":"https://hooks.example/x"}}`)

	for _, tt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/channels/" + ch.ID, ""},
		{http.MethodPatch, "/api/v1/channels/" + ch.ID, `{"name":"mine"}`},
		{http.MethodDelete, "/api/v1/channels/" + ch.ID, ""},
		{http.MethodPost, "/api/v1/channels/" + ch.ID + "/test", ""},
		{http.MethodGet, "/api/v1/channels/not-a-uuid", ""},
	} {
		rec := api.do(t, tt.method, tt.path, tt.body, bob)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", tt.method, tt.path, rec.Body.String())
	}

	rec := api.do(t, http.MethodGet, "/api/v1/channels", "", bob)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items": [], "total": 0}`, rec.Body.String())
	assert.Zero(t, api.sender.count())

	rec = api.do(t, http.MethodGet, "/api/v1/channels/"+ch.ID, "", alice)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ops", decode[channelJSON](t, rec.Body.Bytes()).Name, "bob changed nothing")
}

func TestChannelsRequireAuth(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	for _, path := range []string{"/api/v1/channels", "/api/v1/channels/00000000-0000-0000-0000-000000000000/test"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/test") {
			method = http.MethodPost
		}
		rec := api.do(t, method, path, "", "")
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
}

// listJSON is the envelope of list responses.
type listJSON[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}
