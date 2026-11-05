package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability/obstest"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// catalog is the metric catalog of PROMPT.md §10.1 and docs/metrics.md. The
// names are a contract with the dashboards and alerts in uptime-infra.
var catalog = []string{
	"uptime_http_requests_total",
	"uptime_http_request_duration_seconds",
	"uptime_http_requests_in_flight",
	"uptime_checks_total",
	"uptime_check_duration_seconds",
	"uptime_scheduler_lag_seconds",
	"uptime_scheduler_due_monitors",
	"uptime_worker_pool_busy",
	"uptime_worker_pool_size",
	"uptime_notifications_total",
	"uptime_notification_delivery_delay_seconds",
	"uptime_notifications_pending",
	"uptime_notification_oldest_pending_age_seconds",
	"uptime_incidents_open",
	"uptime_incidents_opened_total",
	"uptime_monitors",
	"uptime_monitors_cert_expiring",
	"uptime_retention_deleted_rows_total",
	"uptime_retention_last_success_timestamp_seconds",
	"uptime_db_pool_acquired_connections",
	"uptime_db_pool_idle_connections",
	"uptime_db_pool_total_connections",
	"uptime_db_pool_max_connections",
	"uptime_db_pool_acquires_total",
	"uptime_db_pool_acquire_waits_total",
	"uptime_db_pool_acquire_wait_seconds_total",
	"uptime_build_info",
}

var testInfo = BuildInfo{Version: "1.2.3", Commit: "abc123", GoVersion: "go1.27.0"}

// lazyPool is a real pgxpool that never connects (nothing listens on port
// 1, and the pool opens connections only on demand).
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://uptime:uptime@127.0.0.1:1/uptime?pool_max_conns=7")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// fullRegistry registers every component, as api and worker do together.
func fullRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := NewRegistry(testInfo)
	reg.MustRegister(NewPoolCollector(lazyPool(t)))
	NewHTTPMetrics(reg).Observe(http.MethodGet, "/version", http.StatusOK, 3*time.Millisecond)
	NewSchedulerMetrics(reg)
	NewNotifierMetrics(reg)
	NewRetentionMetrics(reg)
	NewStatsCollector(reg, fakeStats{}, clock.System{}, slog.New(slog.DiscardHandler), DefaultStatsConfig())
	return reg
}

func TestCatalogIsComplete(t *testing.T) {
	t.Parallel()
	reg := fullRegistry(t)

	families, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]bool{}
	for _, mf := range families {
		got[mf.GetName()] = true
	}
	for _, name := range catalog {
		assert.True(t, got[name], "%s is missing", name)
	}
	assert.True(t, got["go_goroutines"], "Go runtime metrics")
	assert.True(t, got["process_cpu_seconds_total"], "process metrics")
}

// TestLint runs promlint, the linter of the Prometheus project: naming
// conventions, units, help strings, counter suffixes.
func TestLint(t *testing.T) {
	t.Parallel()

	problems, err := testutil.GatherAndLint(fullRegistry(t))

	require.NoError(t, err)
	assert.Empty(t, problems)
}

func TestBuildInfo(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(testInfo)

	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_build_info",
		"version", "1.2.3", "commit", "abc123", "go_version", "go1.27.0"), 0)
}

func TestPoolCollector(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewPoolCollector(lazyPool(t)))

	assert.InDelta(t, 7, obstest.Value(t, reg, "uptime_db_pool_max_connections"), 0)
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_db_pool_acquired_connections"), 0)
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_db_pool_acquire_wait_seconds_total"), 0)
}

func TestHTTPMetricsNormalizeMethod(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewHTTPMetrics(reg)

	m.Observe("BREW", "/version", http.StatusMethodNotAllowed, time.Millisecond)
	m.Observe("PATCH", "/version", http.StatusMethodNotAllowed, time.Millisecond)

	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_http_requests_total",
		"method", MethodOther, "route", "/version", "code", "405"), 0)
	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_http_requests_total",
		"method", "PATCH", "route", "/version", "code", "405"), 0)
}

func TestHTTPMetricsInFlight(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewHTTPMetrics(reg)

	done1, done2 := m.InFlight(), m.InFlight()
	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_http_requests_in_flight"), 0)
	done1()
	done2()
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_http_requests_in_flight"), 0)
}

func TestSchedulerMetricsUnknownKind(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewSchedulerMetrics(reg)

	// A failure without a kind is a bug elsewhere; it must still land in
	// the closed list.
	m.CheckDone(domain.CheckResult{}, time.Second)

	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_checks_total", "result", "failure", "error_kind", "unknown"), 0)
}

func TestHandler(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(testInfo)
	rec := httptest.NewRecorder()

	Handler(reg).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `uptime_build_info{commit="abc123",go_version="go1.27.0",version="1.2.3"} 1`)
}

// fakeStats returns a fixed snapshot or an error.
type fakeStats struct {
	stats domain.SystemStats
	err   error
}

func (f fakeStats) SystemStats(context.Context) (domain.SystemStats, error) { return f.stats, f.err }

func TestStatsCollector(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeStats{stats: domain.SystemStats{
		ActiveMonitors:       5,
		PausedMonitors:       2,
		DueMonitors:          1,
		CertExpiringMonitors: 3,
		OpenIncidents:        4,
		PendingNotifications: 6,
		OldestPendingAt:      now.Add(-90 * time.Second),
	}}
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	c := NewStatsCollector(reg, storeFunc(func() (domain.SystemStats, error) { return store.stats, store.err }),
		clock.NewFake(now), slog.New(slog.NewTextHandler(&logs, nil)), DefaultStatsConfig())

	c.RunOnce(t.Context())

	want := map[string]float64{
		"uptime_scheduler_due_monitors":                  1,
		"uptime_monitors_cert_expiring":                  3,
		"uptime_incidents_open":                          4,
		"uptime_notifications_pending":                   6,
		"uptime_notification_oldest_pending_age_seconds": 90,
	}
	for name, v := range want {
		assert.InDelta(t, v, obstest.Value(t, reg, name), 0, name)
	}
	assert.InDelta(t, 5, obstest.Value(t, reg, "uptime_monitors", "state", "active"), 0)
	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_monitors", "state", "paused"), 0)

	// A failed refresh keeps the last values and is logged.
	store.err = errors.New("connection refused")
	c.RunOnce(t.Context())
	assert.InDelta(t, 4, obstest.Value(t, reg, "uptime_incidents_open"), 0)
	assert.Contains(t, logs.String(), "connection refused")

	// An empty outbox has no oldest notification.
	store.err = nil
	store.stats = domain.SystemStats{}
	c.RunOnce(t.Context())
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_notification_oldest_pending_age_seconds"), 0)
}

// storeFunc adapts a function to StatsStore.
type storeFunc func() (domain.SystemStats, error)

func (f storeFunc) SystemStats(context.Context) (domain.SystemStats, error) { return f() }

func TestStatsCollectorRunStops(t *testing.T) {
	t.Parallel()
	calls := make(chan struct{}, 100)
	store := storeFunc(func() (domain.SystemStats, error) {
		calls <- struct{}{}
		return domain.SystemStats{}, nil
	})
	c := NewStatsCollector(prometheus.NewRegistry(), store, clock.System{}, slog.New(slog.DiscardHandler),
		StatsConfig{Interval: time.Millisecond, Timeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	for range 3 {
		<-calls
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
