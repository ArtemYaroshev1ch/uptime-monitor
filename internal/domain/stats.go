package domain

import "time"

// SystemStats is a snapshot of the whole system for the gauges on the
// worker's /metrics: how big it is and how much work is waiting. Counts are
// over all users.
type SystemStats struct {
	ActiveMonitors int
	PausedMonitors int
	// DueMonitors are active monitors whose next check time has passed: the
	// scheduler's backlog.
	DueMonitors int
	// CertExpiringMonitors are active monitors whose last seen certificate
	// expires within their cert_expiry_warn_days (warned about or not).
	CertExpiringMonitors int
	OpenIncidents        int
	// PendingNotifications are outbox rows not yet sent or failed, including
	// those waiting for a retry.
	PendingNotifications int
	// OldestPendingAt is when the oldest pending notification was created;
	// zero when there is none.
	OldestPendingAt time.Time
}
