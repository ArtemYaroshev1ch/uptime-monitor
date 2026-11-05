package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// StatsStore reads the system snapshot (repository/postgres.Stats).
type StatsStore interface {
	SystemStats(ctx context.Context) (domain.SystemStats, error)
}

// StatsConfig tunes the StatsCollector.
type StatsConfig struct {
	// Interval is how often the snapshot is refreshed.
	Interval time.Duration
	// Timeout bounds one refresh.
	Timeout time.Duration
}

// DefaultStatsConfig refreshes every 15 s, the usual scrape interval: more
// often would only load the database, less often would make the gauges lag
// behind the scrapes.
func DefaultStatsConfig() StatsConfig {
	return StatsConfig{Interval: 15 * time.Second, Timeout: 5 * time.Second}
}

// StatsCollector keeps the gauges that need a database query up to date.
// They are refreshed by a loop, not at scrape time: a scrape must stay fast
// and must not depend on the database, and several Prometheus servers
// scraping would multiply the queries.
type StatsCollector struct {
	store StatsStore
	clock clock.Clock
	log   *slog.Logger
	cfg   StatsConfig

	due, pending, oldestAge, incidents, certExpiring prometheus.Gauge
	monitors                                         *prometheus.GaugeVec
}

// NewStatsCollector registers the snapshot gauges in reg.
func NewStatsCollector(reg prometheus.Registerer, store StatsStore, clk clock.Clock, log *slog.Logger, cfg StatsConfig) *StatsCollector {
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	}
	c := &StatsCollector{
		store: store,
		clock: clk,
		log:   log.With(slog.String("component", "stats")),
		cfg:   cfg,
		due: gauge("scheduler_due_monitors",
			"Active monitors whose check is due or overdue (scheduler backlog)."),
		pending: gauge("notifications_pending",
			"Notifications not yet delivered or given up on (outbox size)."),
		oldestAge: gauge("notification_oldest_pending_age_seconds",
			"Age of the oldest pending notification; 0 when the outbox is empty."),
		incidents: gauge("incidents_open", "Incidents not yet resolved."),
		certExpiring: gauge("monitors_cert_expiring",
			"Active monitors whose certificate expires within their warning period."),
		monitors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "monitors",
			Help:      "Monitors by state (active, paused).",
		}, []string{"state"}),
	}
	c.monitors.WithLabelValues("active")
	c.monitors.WithLabelValues("paused")
	reg.MustRegister(c.due, c.pending, c.oldestAge, c.incidents, c.certExpiring, c.monitors)
	return c
}

// Run refreshes the gauges at once and then every Interval until ctx is
// cancelled. A failed refresh is logged and leaves the previous values; it
// never stops the worker.
func (c *StatsCollector) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		c.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce refreshes the gauges once.
func (c *StatsCollector) RunOnce(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	s, err := c.store.SystemStats(qctx)
	if err != nil {
		if ctx.Err() == nil {
			c.log.WarnContext(ctx, "refresh metrics snapshot", slog.Any("error", err))
		}
		return
	}

	c.due.Set(float64(s.DueMonitors))
	c.pending.Set(float64(s.PendingNotifications))
	c.incidents.Set(float64(s.OpenIncidents))
	c.certExpiring.Set(float64(s.CertExpiringMonitors))
	c.monitors.WithLabelValues("active").Set(float64(s.ActiveMonitors))
	c.monitors.WithLabelValues("paused").Set(float64(s.PausedMonitors))
	age := 0.0
	if !s.OldestPendingAt.IsZero() {
		age = max(c.clock.Now().Sub(s.OldestPendingAt), 0).Seconds()
	}
	c.oldestAge.Set(age)
}
