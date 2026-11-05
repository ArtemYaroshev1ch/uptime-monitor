//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/repository/postgres"
)

func TestSystemStats(t *testing.T) {
	env := newChecksEnv(t)
	stats := postgres.NewStats(env.pool)
	ctx := t.Context()

	empty, err := stats.SystemStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.SystemStats{}, empty, "an empty database")

	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	down := env.monitor(t, "down", true, past)
	env.monitor(t, "due", true, past)
	expiring := env.monitor(t, "expiring", true, future)
	env.monitor(t, "paused due", false, past)
	pausedExpiring := env.monitor(t, "paused expiring", false, future)
	// cert_expiry_warn_days defaults to 14: 3 days left is expiring, 60 not.
	setCert := func(id any, left time.Duration) {
		_, err := env.pool.Exec(ctx, `UPDATE monitors SET tls_not_after = $2 WHERE id = $1`, id, time.Now().Add(left))
		require.NoError(t, err)
	}
	setCert(expiring.ID, 3*24*time.Hour)
	setCert(pausedExpiring.ID, 3*24*time.Hour)
	setCert(down.ID, 60*24*time.Hour)

	// Three failures open an incident and queue one notification per
	// active channel.
	env.channel(t, env.user.ID, telegram("tg", "1", true))
	for i := range 3 {
		env.save(t, down.ID, failure(time.Now().Add(time.Duration(i-10)*time.Minute), domain.ErrorTimeout))
	}
	// One more pending notification, older, and one already sent.
	created := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
	_, err = env.pool.Exec(ctx, `
		INSERT INTO notifications (monitor_id, channel_id, event, created_at)
		SELECT $1, id, 'cert_expiring', $2 FROM notification_channels`, expiring.ID, created)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `
		INSERT INTO notifications (monitor_id, channel_id, event, status, created_at)
		SELECT $1, id, 'cert_expiring', 'sent', $2 FROM notification_channels`, expiring.ID, created.Add(-time.Hour))
	require.NoError(t, err)

	got, err := stats.SystemStats(ctx)
	require.NoError(t, err)

	assert.Equal(t, 3, got.ActiveMonitors)
	assert.Equal(t, 2, got.PausedMonitors)
	assert.Equal(t, 2, got.DueMonitors, "'down' and 'due'; paused monitors never count")
	assert.Equal(t, 1, got.CertExpiringMonitors, "active only, within the warning period")
	assert.Equal(t, 1, got.OpenIncidents)
	assert.Equal(t, 2, got.PendingNotifications, "sent rows are not pending")
	assert.True(t, created.Equal(got.OldestPendingAt), "oldest pending: %s, want %s", got.OldestPendingAt, created)
}
