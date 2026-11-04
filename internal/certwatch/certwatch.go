// Package certwatch warns about TLS certificates that are about to expire.
// Once an hour it queues a cert_expiring notification for every active
// monitor whose last seen certificate expires within the monitor's
// cert_expiry_warn_days, once per certificate.
package certwatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// Store queues the warnings and records them as sent
// (repository/postgres.Notifications.EnqueueCertExpiring). Deduplication
// lives there, in the same statement, so that it holds across restarts and
// worker replicas.
type Store interface {
	EnqueueCertExpiring(ctx context.Context, now time.Time) (domain.CertWarnings, error)
}

// Watcher is the certwatch loop of the worker.
type Watcher struct {
	store    Store
	clock    clock.Clock
	log      *slog.Logger
	interval time.Duration
}

// New returns a Watcher that runs every interval.
func New(store Store, clk clock.Clock, log *slog.Logger, interval time.Duration) *Watcher {
	return &Watcher{store: store, clock: clk, log: log.With(slog.String("component", "certwatch")), interval: interval}
}

// Run checks at once and then every interval until ctx is cancelled. A
// failed run is logged and retried at the next interval; it never stops the
// worker. Every worker replica runs the loop; the store makes that safe.
func (w *Watcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce queues the warnings that are due now.
func (w *Watcher) RunOnce(ctx context.Context) {
	res, err := w.store.EnqueueCertExpiring(ctx, w.clock.Now())
	if err != nil {
		if ctx.Err() == nil {
			w.log.ErrorContext(ctx, "queue certificate warnings", slog.Any("error", err))
		}
		return
	}
	if res.Monitors > 0 {
		w.log.InfoContext(ctx, "certificate warnings queued",
			slog.Int("monitors", res.Monitors),
			slog.Int("notifications", res.Notifications))
	}
}
