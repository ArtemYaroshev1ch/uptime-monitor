// Package retention deletes check results older than the retention period,
// so the checks table does not grow forever.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// Store deletes old checks (repository/postgres.Checks).
type Store interface {
	DeleteBefore(ctx context.Context, cutoff time.Time, batch int) (int64, error)
}

// Config tunes the retention loop.
type Config struct {
	// Keep is how long checks are kept (CHECKS_RETENTION_DAYS).
	Keep time.Duration
	// Interval is how often the loop runs.
	Interval time.Duration
	// Batch is the number of rows deleted per statement.
	Batch int
}

// Retention is the cleanup loop of the worker.
type Retention struct {
	store   Store
	clock   clock.Clock
	log     *slog.Logger
	cfg     Config
	metrics *observability.RetentionMetrics
}

// New returns a Retention.
func New(store Store, metrics *observability.RetentionMetrics, clk clock.Clock, log *slog.Logger, cfg Config) *Retention {
	return &Retention{
		store:   store,
		clock:   clk,
		log:     log.With(slog.String("component", "retention")),
		cfg:     cfg,
		metrics: metrics,
	}
}

// Run cleans up at once and then every Interval until ctx is cancelled. A
// failed run is logged and retried at the next interval; it never stops the
// worker. With several workers every one of them runs the loop: the deletes
// are idempotent, so that only costs a few cheap queries.
func (r *Retention) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce deletes the checks older than Keep.
func (r *Retention) RunOnce(ctx context.Context) {
	cutoff := r.clock.Now().Add(-r.cfg.Keep)
	start := r.clock.Now()
	deleted, err := r.store.DeleteBefore(ctx, cutoff, r.cfg.Batch)
	// Rows of a run that failed halfway are gone too.
	r.metrics.Deleted(deleted)
	if err != nil {
		if ctx.Err() == nil {
			r.log.ErrorContext(ctx, "delete old checks", slog.Any("error", err), slog.Int64("deleted", deleted))
		}
		return
	}
	r.metrics.Succeeded(r.clock.Now())
	r.log.InfoContext(ctx, "old checks deleted",
		slog.Int64("deleted", deleted),
		slog.Time("cutoff", cutoff),
		slog.Duration("took", r.clock.Now().Sub(start)),
	)
}
