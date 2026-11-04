package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Checks stores check results and claims monitors that are due.
type Checks struct {
	db TxDB
}

// NewChecks returns a Checks repository.
func NewChecks(db TxDB) *Checks {
	return &Checks{db: db}
}

// ClaimDue takes up to limit active monitors whose next_check_at has passed
// and moves their next_check_at one interval ahead, in one statement.
//
// FOR UPDATE SKIP LOCKED makes this safe with several workers: rows another
// transaction is claiming at this moment are skipped instead of waited for,
// so two workers never get the same monitor and never block each other. Once
// the statement commits, next_check_at is in the future and the monitor is
// no longer due for anyone.
//
// The CTE keeps next_check_at as it was before the update (scheduled_for):
// comparing it with the actual start of the check gives the scheduler lag.
// The next check is planned from now(), not from scheduled_for: after an
// outage the backlog is checked once, not once per missed interval.
func (r *Checks) ClaimDue(ctx context.Context, limit int) ([]domain.CheckTarget, error) {
	rows, err := r.db.Query(ctx, `
		WITH due AS (
			SELECT id, next_check_at AS scheduled_for
			FROM monitors
			WHERE is_active AND next_check_at <= now()
			ORDER BY next_check_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE monitors m
		SET next_check_at = now() + make_interval(secs => m.interval_seconds)
		FROM due
		WHERE m.id = due.id
		RETURNING m.id, m.url, m.method, m.timeout_seconds, m.expected_status, m.keyword,
		          m.follow_redirects, due.scheduled_for`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("claim due monitors: %w", err)
	}
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.CheckTarget, error) {
		var t domain.CheckTarget
		var timeoutSeconds int
		err := row.Scan(&t.MonitorID, &t.URL, &t.Method, &timeoutSeconds, &t.ExpectedStatus, &t.Keyword,
			&t.FollowRedirects, &t.ScheduledFor)
		t.Timeout = time.Duration(timeoutSeconds) * time.Second
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim due monitors: %w", err)
	}
	return targets, nil
}

// SaveResult stores a check result, applies it to the monitor's health
// (domain.Evaluate), opens or resolves an incident and queues its
// notifications (one per active channel of the owner), all in one
// transaction: the check, the monitor state, the incident and the outbox
// never disagree.
// A monitor deleted while it was being checked returns domain.ErrNotFound.
//
// The monitor row is locked first (FOR UPDATE). Two results of the same
// monitor (possible when a slow check overlaps the next one, or with several
// workers) are therefore evaluated one after the other, each seeing the
// state the previous one left: the second failure at the threshold sees the
// incident the first one opened. The partial unique index on open incidents
// is the last line of defense (ON CONFLICT DO NOTHING).
func (r *Checks) SaveResult(ctx context.Context, monitorID uuid.UUID, res domain.CheckResult) (domain.IncidentChange, error) {
	var change domain.IncidentChange
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var (
			h         domain.Health
			threshold int
		)
		err := tx.QueryRow(ctx, `
			SELECT status, consecutive_failures, failure_threshold,
			       EXISTS (SELECT 1 FROM incidents WHERE monitor_id = m.id AND resolved_at IS NULL)
			FROM monitors m
			WHERE id = $1
			FOR UPDATE`,
			monitorID).Scan(&h.Status, &h.ConsecutiveFailures, &threshold, &h.IncidentOpen)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("monitor deleted: %w", domain.ErrNotFound)
			}
			return fmt.Errorf("lock monitor: %w", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO checks (monitor_id, checked_at, success, status_code, response_time_ms,
				dns_ms, connect_ms, tls_ms, ttfb_ms, http_proto, tls_version, tls_not_after,
				error_kind, error_message)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			monitorID, res.CheckedAt, res.Success, res.StatusCode, res.ResponseTimeMs,
			res.DNSMs, res.ConnectMs, res.TLSMs, res.TTFBMs, nullString(res.HTTPProto),
			nullString(res.TLSVersion), res.TLSNotAfter, nullString(string(res.ErrorKind)),
			nullString(res.ErrorMessage))
		if err != nil {
			return fmt.Errorf("insert check: %w", err)
		}

		tr := domain.Evaluate(h, res.Success, threshold)

		_, err = tx.Exec(ctx, `
			UPDATE monitors SET
				last_checked_at = $2,
				tls_not_after = COALESCE($3, tls_not_after),
				consecutive_failures = $4,
				status = $5
			WHERE id = $1`,
			monitorID, res.CheckedAt, res.TLSNotAfter, tr.Next.ConsecutiveFailures, tr.Next.Status)
		if err != nil {
			return fmt.Errorf("update monitor state: %w", err)
		}

		switch {
		case tr.OpenIncident:
			if change.Opened, err = openIncident(ctx, tx, monitorID, res); err != nil || change.Opened == nil {
				return err
			}
			return enqueueIncident(ctx, tx, monitorID, change.Opened.ID, domain.EventIncidentOpened)
		case tr.ResolveIncident:
			if change.Resolved, err = resolveIncident(ctx, tx, monitorID, res.CheckedAt); err != nil || change.Resolved == nil {
				return err
			}
			return enqueueIncident(ctx, tx, monitorID, change.Resolved.ID, domain.EventIncidentResolved)
		}
		return nil
	})
	if err != nil {
		return domain.IncidentChange{}, fmt.Errorf("save check result: %w", err)
	}
	return change, nil
}

// openIncident opens an incident that started at the first failed check of
// the current streak: the oldest failure after the last success. It returns
// nil if an incident is already open (the unique index refused the insert).
func openIncident(ctx context.Context, tx pgx.Tx, monitorID uuid.UUID, res domain.CheckResult) (*domain.Incident, error) {
	inc, err := scanIncident(tx.QueryRow(ctx, `
		WITH streak AS (
			SELECT min(checked_at) AS started_at
			FROM checks
			WHERE monitor_id = $1 AND NOT success
			  AND checked_at > COALESCE(
			      (SELECT max(checked_at) FROM checks WHERE monitor_id = $1 AND success),
			      '-infinity')
		)
		INSERT INTO incidents AS i (monitor_id, started_at, cause_kind)
		SELECT $1, COALESCE(streak.started_at, $2), $3 FROM streak
		ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING `+incidentColumns,
		monitorID, res.CheckedAt, nullString(string(res.ErrorKind))))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open incident: %w", err)
	}
	return &inc, nil
}

// resolveIncident closes the monitor's open incident at the time of the
// successful check (never before it started).
func resolveIncident(ctx context.Context, tx pgx.Tx, monitorID uuid.UUID, at time.Time) (*domain.Incident, error) {
	inc, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE incidents AS i SET resolved_at = GREATEST(started_at, $2)
		WHERE monitor_id = $1 AND resolved_at IS NULL
		RETURNING `+incidentColumns,
		monitorID, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve incident: %w", err)
	}
	return &inc, nil
}

const checkColumns = `c.id, c.monitor_id, c.checked_at, c.success, c.status_code, c.response_time_ms,
	c.dns_ms, c.connect_ms, c.tls_ms, c.ttfb_ms, c.http_proto, c.tls_version, c.tls_not_after,
	c.error_kind, c.error_message`

// List returns a page of the checks of the user's monitor with from <=
// checked_at < to, newest first, and how many checks the range holds. The
// join on monitors keeps it scoped to the owner.
func (r *Checks) List(ctx context.Context, userID, monitorID uuid.UUID, from, to time.Time, limit, offset int) ([]domain.Check, int, error) {
	const where = `
		FROM checks c JOIN monitors m ON m.id = c.monitor_id
		WHERE c.monitor_id = $1 AND m.user_id = $2 AND c.checked_at >= $3 AND c.checked_at < $4`

	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(*) `+where, monitorID, userID, from, to).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count checks: %w", err)
	}

	rows, err := r.db.Query(ctx, `SELECT `+checkColumns+where+`
		ORDER BY c.checked_at DESC, c.id DESC
		LIMIT $5 OFFSET $6`,
		monitorID, userID, from, to, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list checks: %w", err)
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Check, error) {
		var c domain.Check
		var proto, tlsVersion, kind, message *string
		err := row.Scan(&c.ID, &c.MonitorID, &c.CheckedAt, &c.Success, &c.StatusCode, &c.ResponseTimeMs,
			&c.DNSMs, &c.ConnectMs, &c.TLSMs, &c.TTFBMs, &proto, &tlsVersion, &c.TLSNotAfter,
			&kind, &message)
		c.HTTPProto, c.TLSVersion = deref(proto), deref(tlsVersion)
		c.ErrorKind, c.ErrorMessage = domain.ErrorKind(deref(kind)), deref(message)
		return c, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list checks: %w", err)
	}
	return checks, total, nil
}

// Stats aggregates the checks of the user's monitor since the given time.
// Everything is computed by PostgreSQL in one pass: FILTER restricts an
// aggregate to some rows, percentile_cont interpolates the 95th percentile.
// Latency covers successful checks only.
func (r *Checks) Stats(ctx context.Context, userID, monitorID uuid.UUID, since time.Time) (domain.MonitorStats, error) {
	var s domain.MonitorStats
	err := r.db.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE NOT c.success),
			round(100.0 * count(*) FILTER (WHERE c.success) / NULLIF(count(*), 0), 3)::float8,
			round(avg(c.response_time_ms) FILTER (WHERE c.success), 1)::float8,
			round((percentile_cont(0.95) WITHIN GROUP (ORDER BY c.response_time_ms)
				FILTER (WHERE c.success))::numeric, 1)::float8,
			round((percentile_cont(0.95) WITHIN GROUP (ORDER BY c.ttfb_ms)
				FILTER (WHERE c.success))::numeric, 1)::float8
		FROM checks c JOIN monitors m ON m.id = c.monitor_id
		WHERE c.monitor_id = $1 AND m.user_id = $2 AND c.checked_at >= $3`,
		monitorID, userID, since).
		Scan(&s.TotalChecks, &s.FailedChecks, &s.UptimePercent, &s.AvgResponseMs, &s.P95ResponseMs, &s.P95TTFBMs)
	if err != nil {
		return domain.MonitorStats{}, fmt.Errorf("monitor stats: %w", err)
	}
	return s, nil
}

// DeleteBefore removes checks older than cutoff, at most batch rows per
// statement, and returns how many it removed. Small batches keep each
// transaction short, so retention never holds locks or bloats WAL in one
// huge delete; it simply runs until a batch comes back short.
func (r *Checks) DeleteBefore(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	var total int64
	for {
		tag, err := r.db.Exec(ctx, `
			DELETE FROM checks
			WHERE id IN (SELECT id FROM checks WHERE checked_at < $1 LIMIT $2)`,
			cutoff, batch)
		if err != nil {
			return total, fmt.Errorf("delete old checks: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
