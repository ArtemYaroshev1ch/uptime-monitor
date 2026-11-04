//go:build integration

package postgres_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/repository/postgres"
)

func telegram(name, chatID string, active bool) domain.Channel {
	return domain.Channel{Type: domain.ChannelTelegram, Name: name, IsActive: active,
		Config: domain.ChannelConfig{ChatID: chatID}}
}

func (e checksEnv) channel(t *testing.T, userID uuid.UUID, ch domain.Channel) domain.Channel {
	t.Helper()
	out, err := postgres.NewChannels(e.pool).Create(t.Context(), userID, ch, 100)
	require.NoError(t, err)
	return out
}

type outboxRow struct {
	ChannelID  uuid.UUID
	IncidentID *uuid.UUID
	Event      string
	Status     string
	Attempts   int
	LastError  *string
	SentAt     *time.Time
}

func (e checksEnv) outbox(t *testing.T, monitorID uuid.UUID) []outboxRow {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `
		SELECT channel_id, incident_id, event, status, attempts, last_error, sent_at
		FROM notifications WHERE monitor_id = $1 ORDER BY id`, monitorID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.ChannelID, &r.IncidentID, &r.Event, &r.Status, &r.Attempts, &r.LastError, &r.SentAt))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestChannelsCRUDAndIsolation(t *testing.T) {
	env := newChecksEnv(t)
	ctx := t.Context()
	repo := postgres.NewChannels(env.pool)
	bob, err := postgres.NewUsers(env.pool).Create(ctx, "bob@example.com", "hash")
	require.NoError(t, err)

	hook := domain.Channel{Type: domain.ChannelWebhook, Name: "ops", IsActive: true,
		Config: domain.ChannelConfig{URL: "https://hooks.example/x", Secret: "s3cret"}}
	created, err := repo.Create(ctx, env.user.ID, hook, 2)
	require.NoError(t, err)
	assert.Equal(t, hook.Config, created.Config, "config round-trips through JSONB")
	assert.Equal(t, env.user.ID, created.UserID)

	got, err := repo.Get(ctx, env.user.ID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, got)

	_, err = repo.Get(ctx, bob.ID, created.ID)
	require.ErrorIs(t, err, domain.ErrNotFound, "another user's channel")
	_, err = repo.Update(ctx, bob.ID, created.ID, func(*domain.Channel) error { return nil })
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.ErrorIs(t, repo.Delete(ctx, bob.ID, created.ID), domain.ErrNotFound)

	updated, err := repo.Update(ctx, env.user.ID, created.ID, func(ch *domain.Channel) error {
		ch.Name, ch.IsActive = "ops-2", false
		ch.Config.URL = "https://hooks.example/y"
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ops-2", updated.Name)
	assert.False(t, updated.IsActive)
	assert.Equal(t, "s3cret", updated.Config.Secret, "the secret is kept")

	_, err = repo.Create(ctx, env.user.ID, telegram("tg", "1", true), 2)
	require.NoError(t, err)
	_, err = repo.Create(ctx, env.user.ID, telegram("tg2", "2", true), 2)
	require.ErrorIs(t, err, domain.ErrLimitExceeded)

	items, total, err := repo.List(ctx, env.user.ID, 50, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, items, 2)
	_, total, err = repo.List(ctx, bob.ID, 50, 0)
	require.NoError(t, err)
	assert.Zero(t, total)

	require.NoError(t, repo.Delete(ctx, env.user.ID, created.ID))
	_, err = repo.Get(ctx, env.user.ID, created.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestIncidentNotificationsAreQueued: opening and resolving an incident
// queues one notification per active channel of the owner, in the same
// transaction; inactive channels and other users' channels get nothing.
func TestIncidentNotificationsAreQueued(t *testing.T) {
	env := newChecksEnv(t)
	bob, err := postgres.NewUsers(env.pool).Create(t.Context(), "bob@example.com", "hash")
	require.NoError(t, err)
	tg := env.channel(t, env.user.ID, telegram("tg", "1", true))
	env.channel(t, env.user.ID, telegram("muted", "2", false))
	env.channel(t, bob.ID, telegram("bob", "3", true))
	m := env.monitor(t, "api", true, time.Now())
	t0 := time.Now().Add(-time.Hour)

	env.save(t, m.ID, failure(t0, domain.ErrorTimeout))
	env.save(t, m.ID, failure(t0.Add(time.Minute), domain.ErrorTimeout))
	assert.Empty(t, env.outbox(t, m.ID), "below the threshold nothing is sent")

	opened := env.save(t, m.ID, failure(t0.Add(2*time.Minute), domain.ErrorTimeout)).Opened
	require.NotNil(t, opened)
	env.save(t, m.ID, failure(t0.Add(3*time.Minute), domain.ErrorTimeout))
	resolved := env.save(t, m.ID, success(t0.Add(4*time.Minute))).Resolved
	require.NotNil(t, resolved)

	rows := env.outbox(t, m.ID)
	require.Len(t, rows, 2)
	for i, event := range []string{"incident_opened", "incident_resolved"} {
		assert.Equal(t, event, rows[i].Event)
		assert.Equal(t, tg.ID, rows[i].ChannelID)
		require.NotNil(t, rows[i].IncidentID)
		assert.Equal(t, opened.ID, *rows[i].IncidentID)
		assert.Equal(t, "pending", rows[i].Status)
		assert.Zero(t, rows[i].Attempts)
	}
}

// TestOutboxClaimAndRetries covers the outbox protocol: claim with a lease,
// retries with a due time, failure, success.
func TestOutboxClaimAndRetries(t *testing.T) {
	env := newChecksEnv(t)
	ctx := t.Context()
	outbox := postgres.NewNotifications(env.pool)
	ch := env.channel(t, env.user.ID, telegram("tg", "42", true))
	m := env.monitor(t, "api", true, time.Now())
	at := time.Now().Add(-time.Minute)
	env.save(t, m.ID, failure(at, domain.ErrorDNS))
	env.save(t, m.ID, failure(at, domain.ErrorDNS))
	opened := env.save(t, m.ID, failure(at, domain.ErrorDNS)).Opened
	require.NotNil(t, opened)

	claimed, err := outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	n := claimed[0]
	assert.Zero(t, n.Attempts)
	assert.Equal(t, n.ID, n.Message.ID)
	assert.Equal(t, domain.EventIncidentOpened, n.Message.Event)
	assert.Equal(t, ch.ID, n.Channel.ID)
	assert.Equal(t, "42", n.Channel.Config.ChatID)
	assert.True(t, n.Channel.IsActive)
	assert.Equal(t, m.ID, n.Message.Monitor.ID)
	assert.Equal(t, m.Name, n.Message.Monitor.Name)
	assert.Equal(t, m.URL, n.Message.Monitor.URL)
	require.NotNil(t, n.Message.Incident)
	assert.Equal(t, opened.ID, n.Message.Incident.ID)
	assert.Equal(t, domain.ErrorDNS, n.Message.Incident.CauseKind)
	assert.Nil(t, n.Message.CertNotAfter)

	again, err := outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, again, "leased: not claimed twice")

	// A retry in the past is due at once.
	require.NoError(t, outbox.MarkRetry(ctx, n.ID, time.Now().Add(-time.Second), "telegram responded 502"))
	again, err = outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, 1, again[0].Attempts)

	// A retry in the future is not.
	require.NoError(t, outbox.MarkRetry(ctx, n.ID, time.Now().Add(time.Hour), "telegram responded 502"))
	again, err = outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, again)

	require.NoError(t, outbox.MarkFailed(ctx, n.ID, "chat not found"))
	rows := env.outbox(t, m.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Status)
	assert.Equal(t, 3, rows[0].Attempts)
	require.NotNil(t, rows[0].LastError)
	assert.Equal(t, "chat not found", *rows[0].LastError)

	// Resolve: a new notification, delivered at once.
	resolved := env.save(t, m.ID, success(time.Now())).Resolved
	require.NotNil(t, resolved)
	claimed, err = outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NotNil(t, claimed[0].Message.Incident.ResolvedAt)
	sentAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, outbox.MarkSent(ctx, claimed[0].ID, sentAt))
	rows = env.outbox(t, m.ID)
	require.Len(t, rows, 2)
	assert.Equal(t, "sent", rows[1].Status)
	assert.Equal(t, 1, rows[1].Attempts)
	require.NotNil(t, rows[1].SentAt)
	assert.True(t, sentAt.Equal(*rows[1].SentAt))
}

// TestOutboxConcurrentClaims: several notifiers claiming at once never get
// the same notification.
func TestOutboxConcurrentClaims(t *testing.T) {
	env := newChecksEnv(t)
	ctx := t.Context()
	outbox := postgres.NewNotifications(env.pool)
	ch := env.channel(t, env.user.ID, telegram("tg", "1", true))
	m := env.monitor(t, "api", true, time.Now())
	const total = 60
	for range total {
		_, err := env.pool.Exec(ctx, `
			INSERT INTO notifications (monitor_id, channel_id, event) VALUES ($1, $2, 'cert_expiring')`,
			m.ID, ch.ID)
		require.NoError(t, err)
	}

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for range 6 {
		wg.Go(func() {
			for {
				batch, err := outbox.Claim(ctx, 7, time.Minute)
				if !assert.NoError(t, err) || len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, n := range batch {
					seen[n.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	assert.Len(t, seen, total, "every notification is claimed")
	for id, n := range seen {
		assert.Equal(t, 1, n, "notification %d claimed %d times", id, n)
	}
}

// TestCertWarningsAreDeduplicated is the certwatch guarantee: one warning
// per certificate, a new one after renewal, nothing for certificates far
// from expiry, paused monitors or monitors without TLS.
func TestCertWarningsAreDeduplicated(t *testing.T) {
	env := newChecksEnv(t)
	ctx := t.Context()
	outbox := postgres.NewNotifications(env.pool)
	env.channel(t, env.user.ID, telegram("tg", "1", true))
	env.channel(t, env.user.ID, telegram("tg2", "2", true))
	now := time.Now().UTC().Truncate(time.Microsecond)

	setCert := func(m domain.Monitor, notAfter *time.Time) {
		t.Helper()
		_, err := env.pool.Exec(ctx, `UPDATE monitors SET tls_not_after = $2 WHERE id = $1`, m.ID, notAfter)
		require.NoError(t, err)
	}
	soon := now.Add(5 * 24 * time.Hour)   // within the default 14 days
	later := now.Add(60 * 24 * time.Hour) // outside
	expiring := env.monitor(t, "expiring", true, now)
	setCert(expiring, &soon)
	healthy := env.monitor(t, "healthy", true, now)
	setCert(healthy, &later)
	paused := env.monitor(t, "paused", false, now)
	setCert(paused, &soon)
	env.monitor(t, "plain-http", true, now) // tls_not_after stays NULL

	w, err := outbox.EnqueueCertExpiring(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, domain.CertWarnings{Monitors: 1, Notifications: 2}, w, "one monitor, two channels")

	rows := env.outbox(t, expiring.ID)
	require.Len(t, rows, 2)
	assert.Equal(t, "cert_expiring", rows[0].Event)
	assert.Nil(t, rows[0].IncidentID)

	// The next hourly runs: the same certificate is not warned about again.
	for range 3 {
		w, err = outbox.EnqueueCertExpiring(ctx, now.Add(time.Hour))
		require.NoError(t, err)
		assert.Zero(t, w)
	}

	// The claimed message carries the expiry that was warned about.
	claimed, err := outbox.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	require.NotNil(t, claimed[0].Message.CertNotAfter)
	assert.True(t, soon.Equal(*claimed[0].Message.CertNotAfter))
	assert.Nil(t, claimed[0].Message.Incident)

	// A checked result with the same certificate keeps the deduplication;
	// a renewed (but still soon expiring) certificate is warned about anew.
	renewed := now.Add(10 * 24 * time.Hour)
	_, err = env.checks.SaveResult(ctx, expiring.ID, domain.CheckResult{
		CheckedAt: now, Success: true, ResponseTimeMs: 10, TLSNotAfter: &renewed,
	})
	require.NoError(t, err)
	w, err = outbox.EnqueueCertExpiring(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, 1, w.Monitors)
	assert.Len(t, env.outbox(t, expiring.ID), 4)

	// A certificate moving into the window later is warned about then.
	w, err = outbox.EnqueueCertExpiring(ctx, now.Add(50*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, w.Monitors, "healthy is now within 14 days")
	assert.Len(t, env.outbox(t, healthy.ID), 2)
	assert.Empty(t, env.outbox(t, paused.ID))
}
