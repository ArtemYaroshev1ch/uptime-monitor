package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Stats reads the system-wide snapshot behind the worker's gauges.
type Stats struct {
	db DB
}

// NewStats returns a Stats repository.
func NewStats(db DB) *Stats {
	return &Stats{db: db}
}

// SystemStats counts monitors, open incidents and pending notifications in
// one round trip. The monitors are scanned in full (cheap for the sizes this
// service is built for); incidents and notifications use their partial
// indexes on open and pending rows. The "expiring" condition is the one
// certwatch uses, without the "already warned" part.
func (r *Stats) SystemStats(ctx context.Context) (domain.SystemStats, error) {
	var s domain.SystemStats
	var oldest *time.Time
	err := r.db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE m.is_active),
			count(*) FILTER (WHERE NOT m.is_active),
			count(*) FILTER (WHERE m.is_active AND m.next_check_at <= now()),
			count(*) FILTER (WHERE m.is_active AND m.tls_not_after IS NOT NULL
				AND m.tls_not_after < now() + make_interval(days => m.cert_expiry_warn_days)),
			(SELECT count(*) FROM incidents WHERE resolved_at IS NULL),
			(SELECT count(*) FROM notifications WHERE status = 'pending'),
			(SELECT min(created_at) FROM notifications WHERE status = 'pending')
		FROM monitors m`,
	).Scan(&s.ActiveMonitors, &s.PausedMonitors, &s.DueMonitors, &s.CertExpiringMonitors,
		&s.OpenIncidents, &s.PendingNotifications, &oldest)
	if err != nil {
		return domain.SystemStats{}, fmt.Errorf("read system stats: %w", err)
	}
	if oldest != nil {
		s.OldestPendingAt = *oldest
	}
	return s, nil
}
