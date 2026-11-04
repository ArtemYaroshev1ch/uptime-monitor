package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/checker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/repository/memory"
)

// fakeSender records the messages it is asked to send and returns err.
type fakeSender struct {
	err  error
	sent []domain.Message
	to   []domain.Channel
}

func (f *fakeSender) Send(_ context.Context, ch domain.Channel, msg domain.Message) error {
	f.sent = append(f.sent, msg)
	f.to = append(f.to, ch)
	return f.err
}

type channelsEnv struct {
	svc    *Channels
	repo   *memory.Channels
	sender *fakeSender
	user   uuid.UUID
}

func newChannelsEnv(t *testing.T) channelsEnv {
	t.Helper()
	clk := clock.NewFake(testNow)
	repo := memory.NewChannels(clk.Now)
	sender := &fakeSender{}
	return channelsEnv{
		svc:    NewChannels(repo, checker.NewTargetPolicy(false), sender, clk),
		repo:   repo,
		sender: sender,
		user:   uuid.New(),
	}
}

func channelPatch(t *testing.T, body string) ChannelPatch {
	t.Helper()
	var p ChannelPatch
	require.NoError(t, json.Unmarshal([]byte(body), &p))
	return p
}

func TestCreateChannel(t *testing.T) {
	t.Parallel()
	env := newChannelsEnv(t)

	tg, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":" me ","config":{"chat_id":" -100123 "}}`))
	require.NoError(t, err)
	assert.Equal(t, domain.ChannelTelegram, tg.Type)
	assert.Equal(t, "me", tg.Name)
	assert.Equal(t, domain.ChannelConfig{ChatID: "-100123"}, tg.Config)
	assert.True(t, tg.IsActive, "active by default")

	wh, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"webhook","name":"ops","config":{"url":"https://hooks.example/x"},"is_active":false}`))
	require.NoError(t, err)
	assert.Equal(t, "https://hooks.example/x", wh.Config.URL)
	assert.Len(t, wh.Config.Secret, 26, "a generated 128-bit secret")
	assert.False(t, wh.IsActive)

	other, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"webhook","name":"ops2","config":{"url":"https://hooks.example/y"}}`))
	require.NoError(t, err)
	assert.NotEqual(t, wh.Config.Secret, other.Config.Secret, "every webhook has its own secret")
}

func TestCreateChannelValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []domain.FieldError
	}{
		{
			name: "empty",
			body: `{}`,
			want: []domain.FieldError{{Field: "name", Issue: "is required"}, {Field: "type", Issue: "is required"}},
		},
		{
			name: "unknown type",
			body: `{"type":"email","name":"x"}`,
			want: []domain.FieldError{{Field: "type", Issue: "must be one of: telegram webhook"}},
		},
		{
			name: "missing config",
			body: `{"type":"telegram","name":"x"}`,
			want: []domain.FieldError{{Field: "config", Issue: "is required"}},
		},
		{
			name: "telegram without chat",
			body: `{"type":"telegram","name":"x","config":{"chat_id":""}}`,
			want: []domain.FieldError{{Field: "config.chat_id", Issue: "is required"}},
		},
		{
			name: "telegram chat with spaces",
			body: `{"type":"telegram","name":"x","config":{"chat_id":"a b"}}`,
			want: []domain.FieldError{{Field: "config.chat_id", Issue: "must be a chat ID or @username"}},
		},
		{
			name: "telegram with url",
			body: `{"type":"telegram","name":"x","config":{"chat_id":"1","url":"https://x.example"}}`,
			want: []domain.FieldError{{Field: "config.url", Issue: "is not used by telegram channels"}},
		},
		{
			name: "webhook without url",
			body: `{"type":"webhook","name":"x","config":{}}`,
			want: []domain.FieldError{{Field: "config.url", Issue: "is required"}},
		},
		{
			name: "webhook to a private address",
			body: `{"type":"webhook","name":"x","config":{"url":"http://169.254.169.254/latest"}}`,
			want: []domain.FieldError{{Field: "config.url", Issue: checker.ErrURLBlocked.Error()}},
		},
		{
			name: "webhook with a bad scheme and a chat",
			body: `{"type":"webhook","name":"x","config":{"url":"ftp://x.example","chat_id":"1"}}`,
			want: []domain.FieldError{
				{Field: "config.chat_id", Issue: "is not used by webhook channels"},
				{Field: "config.url", Issue: checker.ErrURLScheme.Error()},
			},
		},
		{
			name: "long name and null is_active",
			body: `{"type":"telegram","name":"` + strings.Repeat("a", 101) + `","config":{"chat_id":"1"},"is_active":null}`,
			want: []domain.FieldError{
				{Field: "is_active", Issue: "must not be null"},
				{Field: "name", Issue: "must be at most 100 characters"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newChannelsEnv(t)
			_, err := env.svc.Create(t.Context(), env.user, channelPatch(t, tt.body))

			verr, ok := errors.AsType[*domain.ValidationError](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tt.want, verr.Details)
		})
	}
}

func TestChannelLimit(t *testing.T) {
	t.Parallel()
	env := newChannelsEnv(t)
	for range MaxChannelsPerUser {
		_, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":"x","config":{"chat_id":"1"}}`))
		require.NoError(t, err)
	}
	_, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":"x","config":{"chat_id":"1"}}`))
	require.ErrorIs(t, err, domain.ErrLimitExceeded)
}

func TestUpdateChannel(t *testing.T) {
	t.Parallel()
	env := newChannelsEnv(t)
	wh, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"webhook","name":"ops","config":{"url":"https://hooks.example/x"}}`))
	require.NoError(t, err)

	updated, err := env.svc.Update(t.Context(), env.user, wh.ID, channelPatch(t, `{"is_active":false}`))
	require.NoError(t, err)
	assert.False(t, updated.IsActive)
	assert.Equal(t, wh.Config, updated.Config, "absent config is kept")

	updated, err = env.svc.Update(t.Context(), env.user, wh.ID, channelPatch(t, `{"name":"ops-2","config":{"url":"https://hooks.example/y"}}`))
	require.NoError(t, err)
	assert.Equal(t, "ops-2", updated.Name)
	assert.Equal(t, "https://hooks.example/y", updated.Config.URL)
	assert.Equal(t, wh.Config.Secret, updated.Config.Secret, "the secret survives a URL change")

	_, err = env.svc.Update(t.Context(), env.user, wh.ID, channelPatch(t, `{"type":"telegram","config":{"url":null}}`))
	verr, ok := errors.AsType[*domain.ValidationError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, []domain.FieldError{
		{Field: "config.url", Issue: "is required"},
		{Field: "type", Issue: "cannot be changed"},
	}, verr.Details)
	got, err := env.svc.Get(t.Context(), env.user, wh.ID)
	require.NoError(t, err)
	assert.Equal(t, "ops-2", got.Name, "a rejected update changes nothing")

	_, err = env.svc.Update(t.Context(), uuid.New(), wh.ID, channelPatch(t, `{"name":"x"}`))
	require.ErrorIs(t, err, domain.ErrNotFound, "another user's channel")
}

func TestTestChannel(t *testing.T) {
	t.Parallel()
	env := newChannelsEnv(t)
	ch, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":"me","config":{"chat_id":"1"},"is_active":false}`))
	require.NoError(t, err)

	res, err := env.svc.Test(t.Context(), env.user, ch.ID)
	require.NoError(t, err)
	assert.Equal(t, TestResult{Delivered: true}, res, "disabled channels can be tested too")
	require.Len(t, env.sender.sent, 1)
	assert.Equal(t, domain.EventTest, env.sender.sent[0].Event)
	assert.Equal(t, "me", env.sender.sent[0].ChannelName)
	assert.Equal(t, testNow, env.sender.sent[0].At)
	assert.Equal(t, ch.ID, env.sender.to[0].ID)

	env.sender.err = errors.New("telegram responded 400 Bad Request: chat not found")
	res, err = env.svc.Test(t.Context(), env.user, ch.ID)
	require.NoError(t, err, "a failed delivery is a result, not an error")
	assert.Equal(t, TestResult{Error: "telegram responded 400 Bad Request: chat not found"}, res)

	_, err = env.svc.Test(t.Context(), uuid.New(), ch.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Len(t, env.sender.sent, 2, "nothing is sent for another user's channel")
}

func TestListAndDeleteChannels(t *testing.T) {
	t.Parallel()
	env := newChannelsEnv(t)
	a, err := env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":"a","config":{"chat_id":"1"}}`))
	require.NoError(t, err)
	_, err = env.svc.Create(t.Context(), env.user, channelPatch(t, `{"type":"telegram","name":"b","config":{"chat_id":"2"}}`))
	require.NoError(t, err)

	page, err := env.svc.List(t.Context(), env.user, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "b", page.Items[0].Name, "newest first")

	require.NoError(t, env.svc.Delete(t.Context(), env.user, a.ID))
	require.ErrorIs(t, env.svc.Delete(t.Context(), env.user, a.ID), domain.ErrNotFound)

	env.repo.FailWith(errors.New("db down"))
	_, err = env.svc.List(t.Context(), env.user, 10, 0)
	require.Error(t, err)
}
