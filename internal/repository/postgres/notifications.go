package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Notifications is the outbox: rows are added in the transactions that
// change incidents (enqueueIncident) or warn about certificates
// (EnqueueCertExpiring), and the notifier claims and delivers them.
type Notifications struct {
	db TxDB
}

// NewNotifications returns a Notifications repository.
func NewNotifications(db TxDB) *Notifications {
	return &Notifications{db: db}
}

// enqueueIncident adds one notification per active channel of the
// monitor's owner. It runs inside the transaction that opened or resolved
// the incident: the notification exists if and only if the change does.
func enqueueIncident(ctx context.Context, tx pgx.Tx, monitorID, incidentID uuid.UUID, event domain.Event) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO notifications (monitor_id, incident_id, channel_id, event)
		SELECT m.id, $2, c.id, $3
		FROM monitors m
		JOIN notification_channels c ON c.user_id = m.user_id AND c.is_active
		WHERE m.id = $1`,
		monitorID, incidentID, event)
	if err != nil {
		return fmt.Errorf("enqueue %s notifications: %w", event, err)
	}
	return nil
}

// Claim takes up to limit pending notifications that are due and moves
// their next_attempt_at lease ahead, in one statement, returning everything
// needed to deliver them.
//
// It works like Checks.ClaimDue: FOR UPDATE SKIP LOCKED lets several
// workers claim at once without taking the same row or waiting for each
// other, and once the statement commits the rows are not due for anyone
// until the lease ends. The delivery itself happens outside any
// transaction (no row lock is held during a slow HTTP call); if the worker
// dies before recording the outcome, the row becomes due again when the
// lease ends and is delivered once more (at-least-once).
func (r *Notifications) Claim(ctx context.Context, limit int, lease time.Duration) ([]domain.Notification, error) {
	rows, err := r.db.Query(ctx, `
		WITH due AS (
			SELECT id FROM notifications
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE notifications n
			SET next_attempt_at = now() + make_interval(secs => $2)
			FROM due
			WHERE n.id = due.id
			RETURNING n.id, n.monitor_id, n.incident_id, n.channel_id, n.event, n.attempts, n.created_at
		)
		SELECT cl.id, cl.event, cl.attempts, cl.created_at,
		       `+channelColumns+`,
		       m.id, m.name, m.url, m.cert_alerted_for,
		       i.id, i.started_at, i.resolved_at, i.cause_kind
		FROM claimed cl
		JOIN notification_channels c ON c.id = cl.channel_id
		JOIN monitors m ON m.id = cl.monitor_id
		LEFT JOIN incidents i ON i.id = cl.incident_id
		ORDER BY cl.id`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) {
		var (
			n          domain.Notification
			certAfter  *time.Time
			incID      *uuid.UUID
			incStart   *time.Time
			incResolve *time.Time
			incCause   *string
		)
		err := row.Scan(&n.ID, &n.Message.Event, &n.Attempts, &n.CreatedAt,
			&n.Channel.ID, &n.Channel.UserID, &n.Channel.Type, &n.Channel.Name, &n.Channel.Config,
			&n.Channel.IsActive, &n.Channel.CreatedAt,
			&n.Message.Monitor.ID, &n.Message.Monitor.Name, &n.Message.Monitor.URL, &certAfter,
			&incID, &incStart, &incResolve, &incCause)
		n.Message.ID = n.ID
		if incID != nil && incStart != nil {
			n.Message.Incident = &domain.Incident{
				ID:          *incID,
				MonitorID:   n.Message.Monitor.ID,
				MonitorName: n.Message.Monitor.Name,
				StartedAt:   *incStart,
				ResolvedAt:  incResolve,
				CauseKind:   domain.ErrorKind(deref(incCause)),
			}
		}
		if n.Message.Event == domain.EventCertExpiring {
			// The expiry that was warned about (see EnqueueCertExpiring).
			n.Message.CertNotAfter = certAfter
		}
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	return out, nil
}

// MarkSent records a successful delivery.
func (r *Notifications) MarkSent(ctx context.Context, id int64, at time.Time) error {
	return r.mark(ctx, "mark notification sent", `
		UPDATE notifications
		SET status = 'sent', attempts = attempts + 1, sent_at = $2, last_error = NULL
		WHERE id = $1`, id, at)
}

// MarkRetry records a failed attempt and when to try again.
func (r *Notifications) MarkRetry(ctx context.Context, id int64, next time.Time, lastErr string) error {
	return r.mark(ctx, "mark notification for retry", `
		UPDATE notifications
		SET attempts = attempts + 1, next_attempt_at = $2, last_error = $3
		WHERE id = $1`, id, next, lastErr)
}

// MarkFailed records the last failed attempt; the notification is not
// delivered any more.
func (r *Notifications) MarkFailed(ctx context.Context, id int64, lastErr string) error {
	return r.mark(ctx, "mark notification failed", `
		UPDATE notifications
		SET status = 'failed', attempts = attempts + 1, last_error = $2
		WHERE id = $1`, id, lastErr)
}

// mark runs an UPDATE of one outbox row. A row that is gone (its monitor or
// channel was deleted meanwhile) is not an error: there is nothing left to
// record.
func (r *Notifications) mark(ctx context.Context, op, sql string, args ...any) error {
	if _, err := r.db.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// EnqueueCertExpiring finds active monitors whose certificate expires
// within their cert_expiry_warn_days of now and that have not been warned
// about this very certificate, and in one statement (hence one
// transaction) marks them as warned and adds cert_expiring notifications.
//
// Deduplication is by value: cert_alerted_for remembers the tls_not_after
// that was warned about. The same certificate is never warned about twice;
// a renewed one has a new tls_not_after, so its expiry is warned about in
// turn. IS DISTINCT FROM treats NULL (never warned) as different from any
// date. SKIP LOCKED leaves monitors whose check result is being saved right
// now to the next run, and keeps two workers from warning twice.
func (r *Notifications) EnqueueCertExpiring(ctx context.Context, now time.Time) (domain.CertWarnings, error) {
	var w domain.CertWarnings
	err := r.db.QueryRow(ctx, `
		WITH expiring AS (
			SELECT id FROM monitors
			WHERE is_active
			  AND tls_not_after IS NOT NULL
			  AND tls_not_after < $1::timestamptz + make_interval(days => cert_expiry_warn_days)
			  AND cert_alerted_for IS DISTINCT FROM tls_not_after
			FOR UPDATE SKIP LOCKED
		), warned AS (
			UPDATE monitors m SET cert_alerted_for = m.tls_not_after
			FROM expiring
			WHERE m.id = expiring.id
			RETURNING m.id, m.user_id
		), queued AS (
			INSERT INTO notifications (monitor_id, channel_id, event)
			SELECT warned.id, c.id, 'cert_expiring'
			FROM warned
			JOIN notification_channels c ON c.user_id = warned.user_id AND c.is_active
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM warned), (SELECT count(*) FROM queued)`,
		now).Scan(&w.Monitors, &w.Notifications)
	if err != nil {
		return domain.CertWarnings{}, fmt.Errorf("enqueue certificate warnings: %w", err)
	}
	return w, nil
}
