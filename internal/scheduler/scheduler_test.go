package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability/obstest"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
)

// fakeStore hands out queued targets (or an endless supply) and records
// saved results.
type fakeStore struct {
	mu       sync.Mutex
	queue    []domain.CheckTarget
	endless  bool
	claimErr error
	saveErr  error
	change   domain.IncidentChange
	limits   []int
	saved    map[uuid.UUID]domain.CheckResult
}

func newFakeStore(targets ...domain.CheckTarget) *fakeStore {
	return &fakeStore{queue: targets, saved: map[uuid.UUID]domain.CheckResult{}}
}

func (f *fakeStore) ClaimDue(_ context.Context, limit int) ([]domain.CheckTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.endless {
		out := make([]domain.CheckTarget, limit)
		for i := range out {
			out[i] = domain.CheckTarget{MonitorID: uuid.New()}
		}
		return out, nil
	}
	n := min(limit, len(f.queue))
	out := f.queue[:n]
	f.queue = f.queue[n:]
	return out, nil
}

func (f *fakeStore) SaveResult(_ context.Context, id uuid.UUID, res domain.CheckResult) (domain.IncidentChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return domain.IncidentChange{}, f.saveErr
	}
	f.saved[id] = res
	return f.change, nil
}

func (f *fakeStore) savedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

// fakeChecker succeeds at once, or blocks until release is closed (or its
// context ends) when release is set. It tracks how many checks run at once.
type fakeChecker struct {
	release      chan struct{}
	running, max atomic.Int64
	aborted      atomic.Int64
}

func (f *fakeChecker) Check(ctx context.Context, _ domain.CheckTarget) domain.CheckResult {
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			f.aborted.Add(1)
			return domain.CheckResult{ErrorKind: domain.ErrorTimeout}
		}
	}
	return domain.CheckResult{Success: true}
}

func testConfig() Config {
	return Config{
		Concurrency:     3,
		BatchSize:       50,
		Tick:            5 * time.Millisecond,
		ShutdownTimeout: 5 * time.Second,
		SaveTimeout:     time.Second,
	}
}

// start runs s in the background; the returned function cancels it and
// waits for Run to return.
func start(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newMetrics() *observability.SchedulerMetrics {
	return observability.NewSchedulerMetrics(prometheus.NewRegistry())
}

func TestSchedulerChecksAndSaves(t *testing.T) {
	t.Parallel()
	targets := []domain.CheckTarget{{MonitorID: uuid.New()}, {MonitorID: uuid.New()}, {MonitorID: uuid.New()}, {MonitorID: uuid.New()}}
	store := newFakeStore(targets...)
	s := New(store, &fakeChecker{}, newMetrics(), discard(), clock.System{}, testConfig())

	stop := start(t, s)
	require.Eventually(t, func() bool { return store.savedCount() == len(targets) }, 5*time.Second, 5*time.Millisecond)
	stop()

	for _, tg := range targets {
		assert.True(t, store.saved[tg.MonitorID].Success)
	}
	assert.False(t, s.Heartbeat().IsZero())
}

// resultChecker returns the same result for every target.
type resultChecker domain.CheckResult

func (r resultChecker) Check(context.Context, domain.CheckTarget) domain.CheckResult {
	return domain.CheckResult(r)
}

func TestSchedulerRecordsMetrics(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := newFakeStore(
		domain.CheckTarget{MonitorID: uuid.New(), ScheduledFor: now.Add(-3 * time.Second)},
		domain.CheckTarget{MonitorID: uuid.New(), ScheduledFor: now.Add(-3 * time.Second)},
	)
	store.change = domain.IncidentChange{Opened: &domain.Incident{ID: uuid.New()}}
	reg := prometheus.NewRegistry()
	s := New(store, resultChecker{ErrorKind: domain.ErrorTimeout}, observability.NewSchedulerMetrics(reg), discard(), clock.System{}, testConfig())

	stop := start(t, s)
	require.Eventually(t, func() bool { return store.savedCount() == 2 }, 5*time.Second, time.Millisecond)
	stop() // waits for the checks, so every metric is recorded

	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_checks_total", "result", "failure", "error_kind", "timeout"), 0)
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_checks_total", "result", "success", "error_kind", "none"), 0,
		"pre-created at 0")
	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_check_duration_seconds"), 0)
	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_scheduler_lag_seconds"), 0)
	assert.GreaterOrEqual(t, obstest.Sum(t, reg, "uptime_scheduler_lag_seconds"), 6.0, "each check started at least 3 s late")
	assert.InDelta(t, 2, obstest.Value(t, reg, "uptime_incidents_opened_total"), 0)
	assert.InDelta(t, 3, obstest.Value(t, reg, "uptime_worker_pool_size"), 0)
	assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_worker_pool_busy"), 0, "nothing runs after the stop")
}

// TestSchedulerPoolBusy watches the busy gauge while checks are blocked.
func TestSchedulerPoolBusy(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.endless = true
	checker := &fakeChecker{release: make(chan struct{})}
	reg := prometheus.NewRegistry()
	s := New(store, checker, observability.NewSchedulerMetrics(reg), discard(), clock.System{}, testConfig())

	stop := start(t, s)
	busy := func() float64 { return obstest.Value(t, reg, "uptime_worker_pool_busy") }
	require.Eventually(t, func() bool { return busy() == 3 }, 5*time.Second, time.Millisecond, "the pool fills up")
	close(checker.release)
	stop()
	assert.InDelta(t, 0, busy(), 0)
}

// The scheduler never runs more checks than the pool size and never claims
// more monitors than it has free slots for.
func TestSchedulerRespectsConcurrency(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.endless = true
	checker := &fakeChecker{release: make(chan struct{})}
	cfg := testConfig()
	s := New(store, checker, newMetrics(), discard(), clock.System{}, cfg)

	stop := start(t, s)
	require.Eventually(t, func() bool { return checker.running.Load() == int64(cfg.Concurrency) }, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond) // several more ticks while saturated
	assert.Equal(t, int64(cfg.Concurrency), checker.max.Load())
	close(checker.release)
	stop()

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, limit := range store.limits {
		assert.LessOrEqual(t, limit, cfg.Concurrency)
		assert.Positive(t, limit, "no claim while the pool is full")
	}
}

func TestSchedulerBatchSize(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	cfg := testConfig()
	cfg.Concurrency, cfg.BatchSize = 100, 7
	s := New(store, &fakeChecker{}, newMetrics(), discard(), clock.System{}, cfg)

	stop := start(t, s)
	require.Eventually(t, func() bool { store.mu.Lock(); defer store.mu.Unlock(); return len(store.limits) > 0 }, time.Second, time.Millisecond)
	stop()

	assert.Equal(t, 7, store.limits[0])
}

// Shutdown lets running checks finish and save their results.
func TestSchedulerGracefulShutdown(t *testing.T) {
	t.Parallel()
	store := newFakeStore(domain.CheckTarget{MonitorID: uuid.New()})
	checker := &fakeChecker{release: make(chan struct{})}
	s := New(store, checker, newMetrics(), discard(), clock.System{}, testConfig())
	stop := start(t, s)
	require.Eventually(t, func() bool { return checker.running.Load() == 1 }, 5*time.Second, time.Millisecond)

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Run returned while a check was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(checker.release)
	<-stopped
	assert.Equal(t, 1, store.savedCount(), "the running check was saved")
	assert.Zero(t, checker.aborted.Load())
}

// After ShutdownTimeout, checks that are still running are cancelled.
func TestSchedulerShutdownTimeout(t *testing.T) {
	t.Parallel()
	store := newFakeStore(domain.CheckTarget{MonitorID: uuid.New()})
	checker := &fakeChecker{release: make(chan struct{})} // never released
	cfg := testConfig()
	cfg.ShutdownTimeout = 20 * time.Millisecond
	s := New(store, checker, newMetrics(), discard(), clock.System{}, cfg)
	stop := start(t, s)
	require.Eventually(t, func() bool { return checker.running.Load() == 1 }, 5*time.Second, time.Millisecond)

	stop()

	assert.Equal(t, int64(1), checker.aborted.Load())
}

func TestSchedulerClaimError(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.claimErr = errors.New("connection refused")
	var logs bytes.Buffer
	s := New(store, &fakeChecker{}, newMetrics(), slog.New(slog.NewTextHandler(&logs, nil)), clock.System{}, testConfig())

	stop := start(t, s)
	require.Eventually(t, func() bool { store.mu.Lock(); defer store.mu.Unlock(); return len(store.limits) >= 2 }, time.Second, time.Millisecond)
	stop()

	assert.True(t, s.Heartbeat().IsZero(), "no heartbeat without a successful claim")
	assert.Contains(t, logs.String(), "connection refused")
}

func TestSchedulerDeletedMonitorIsNotAnError(t *testing.T) {
	t.Parallel()
	store := newFakeStore(domain.CheckTarget{MonitorID: uuid.New()})
	store.saveErr = domain.ErrNotFound
	var logs bytes.Buffer
	s := New(store, &fakeChecker{}, newMetrics(), slog.New(slog.NewTextHandler(&logs, nil)), clock.System{}, testConfig())

	stop := start(t, s)
	require.Eventually(t, func() bool { store.mu.Lock(); defer store.mu.Unlock(); return len(store.queue) == 0 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	stop()

	assert.NotContains(t, logs.String(), "level=ERROR")
}

func TestHeartbeatUsesClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(newFakeStore(), &fakeChecker{}, newMetrics(), discard(), clock.NewFake(now), testConfig())

	s.beat()

	assert.True(t, now.Equal(s.Heartbeat()))
}

func TestSchedulerLogsIncidents(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolved := started.Add(90 * time.Second)
	incident := domain.Incident{ID: uuid.New(), StartedAt: started, CauseKind: domain.ErrorTimeout}

	for _, tt := range []struct {
		name   string
		change domain.IncidentChange
		want   []string
	}{
		{name: "opened", change: domain.IncidentChange{Opened: &incident},
			want: []string{"level=WARN", `msg="incident opened"`, "incident_id=" + incident.ID.String(), "cause_kind=timeout"}},
		{name: "resolved", change: domain.IncidentChange{Resolved: &domain.Incident{ID: incident.ID, StartedAt: started, ResolvedAt: &resolved}},
			want: []string{"level=INFO", `msg="incident resolved"`, "duration_seconds=90"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := domain.CheckTarget{MonitorID: uuid.New()}
			store := newFakeStore(target)
			store.change = tt.change
			var logs syncBuffer
			// The project logger adds the context attributes (monitor_id).
			log := logger.New(&logs, logger.Options{Format: logger.FormatText, Level: slog.LevelInfo})
			s := New(store, &fakeChecker{}, newMetrics(), log, clock.System{}, testConfig())

			stop := start(t, s)
			require.Eventually(t, func() bool { return store.savedCount() == 1 }, 5*time.Second, time.Millisecond)
			stop()

			for _, w := range tt.want {
				assert.Contains(t, logs.String(), w)
			}
			assert.Contains(t, logs.String(), "monitor_id="+target.MonitorID.String())
		})
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger
// shared by goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
